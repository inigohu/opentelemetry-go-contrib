// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otelgrpc // import "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

import (
	"context"
	"sync/atomic"
	"time"

	grpc_codes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc/internal"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

type serverHandler struct {
	*config
}

// NewServerHandler creates a stats.Handler for a gRPC server.
func NewServerHandler(opts ...Option) stats.Handler {
	h := &serverHandler{
		config: newConfig(opts, "server"),
	}

	return h
}

// TagConn can attach some information to the given context.
func (h *serverHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn processes the Conn stats.
func (h *serverHandler) HandleConn(ctx context.Context, info stats.ConnStats) {
}

// TagRPC can attach some information to the given context.
func (h *serverHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = extract(ctx, h.config.Propagators)

	gctx, _ := GRPCContextFromContext(ctx)
	gctx.traceInfo.kind = trace.SpanKindServer

	ctx = h.tagRPC(ctx, info)

	return ctx
}

// HandleRPC processes the RPC stats.
func (h *serverHandler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	h.handleRPC(ctx, rs)
}

type clientHandler struct {
	*config
}

// NewClientHandler creates a stats.Handler for a gRPC client.
func NewClientHandler(opts ...Option) stats.Handler {
	h := &clientHandler{
		config: newConfig(opts, "client"),
	}

	return h
}

// TagRPC can attach some information to the given context.
func (h *clientHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	gctx, _ := GRPCContextFromContext(ctx)
	gctx.traceInfo.kind = trace.SpanKindClient

	ctx = h.tagRPC(ContextWithGRPCContext(ctx, gctx), info)

	return inject(ctx, h.config.Propagators)
}

// HandleRPC processes the RPC stats.
func (h *clientHandler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	h.handleRPC(ctx, rs)
}

// TagConn can attach some information to the given context.
func (h *clientHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn processes the Conn stats.
func (h *clientHandler) HandleConn(context.Context, stats.ConnStats) {
	// no-op
}

func (c *config) tagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	name, attrs := internal.ParseFullMethod(info.FullMethodName)
	attrs = append(attrs, RPCSystemGRPC)

	gctx, _ := GRPCContextFromContext(ctx)
	gctx.traceInfo.name = name
	gctx.attrs = append(gctx.attrs, attrs...)

	return ContextWithGRPCContext(ctx, gctx)
}

func (c *config) handleRPC(ctx context.Context, rs stats.RPCStats) {
	gctx, _ := GRPCContextFromContext(ctx)
	if gctx.traceInfo.kind == trace.SpanKindServer {
		ctx = trace.ContextWithRemoteSpanContext(ctx, trace.SpanContextFromContext(ctx))
	}

	ctx, span := c.tracer.Start(
		ctx,
		gctx.traceInfo.name,
		trace.WithSpanKind(gctx.traceInfo.kind),
	)

	// access these counts atomically for hedging in the future
	// number of messages sent from side (client || server)
	var sentMsgs int64
	// number of messages received on side (client || server)
	var recvMsgs int64

	switch rs := rs.(type) {
	case *stats.Begin:
	case *stats.InPayload:
		if c.ReceivedEvent {
			span.AddEvent("message",
				trace.WithAttributes(
					semconv.MessageTypeReceived,
					semconv.MessageIDKey.Int64(atomic.AddInt64(&recvMsgs, 1)),
					semconv.MessageCompressedSizeKey.Int(rs.CompressedLength),
					semconv.MessageUncompressedSizeKey.Int(rs.Length),
				),
			)
		}
	case *stats.OutPayload:
		if c.SentEvent {
			span.AddEvent("message",
				trace.WithAttributes(
					semconv.MessageTypeSent,
					semconv.MessageIDKey.Int64(atomic.AddInt64(&sentMsgs, 1)),
					semconv.MessageCompressedSizeKey.Int(rs.CompressedLength),
					semconv.MessageUncompressedSizeKey.Int(rs.Length),
				),
			)
		}
	case *stats.OutTrailer:
	case *stats.OutHeader:
		if p, ok := peer.FromContext(ctx); ok {
			span.SetAttributes(peerAttr(p.Addr.String())...)
		}
	case *stats.End:
		if rs.Error != nil {
			s, _ := status.FromError(rs.Error)
			if gctx.traceInfo.kind == trace.SpanKindServer {
				statusCode, msg := serverStatus(s)
				span.SetStatus(statusCode, msg)
			} else {
				span.SetStatus(codes.Error, s.Message())
			}
			gctx.AddAttrs(semconv.RPCGRPCStatusCodeKey.Int(int(s.Code())))
		} else {
			gctx.AddAttrs(semconv.RPCGRPCStatusCodeKey.Int(int(grpc_codes.OK)))
		}

		span.SetAttributes(gctx.attrs...)
		span.End()

		// Use floating point division here for higher precision (instead of Millisecond method).
		elapsedTime := float64(rs.EndTime.Sub(rs.BeginTime)) / float64(time.Millisecond)

		c.rpcDuration.Record(ctx, elapsedTime, metric.WithAttributes(gctx.attrs...))
		c.rpcRequestSize.Record(ctx, atomic.LoadInt64(&recvMsgs), metric.WithAttributes(gctx.attrs...))
		c.rpcResponseSize.Record(ctx, atomic.LoadInt64(&sentMsgs), metric.WithAttributes(gctx.attrs...))
		c.rpcRequestsPerRPC.Record(ctx, atomic.LoadInt64(&recvMsgs), metric.WithAttributes(gctx.attrs...))
		c.rpcResponsesPerRPC.Record(ctx, atomic.LoadInt64(&sentMsgs), metric.WithAttributes(gctx.attrs...))
	default:
		return
	}
}
