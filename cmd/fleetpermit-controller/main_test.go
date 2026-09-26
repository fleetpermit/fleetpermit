/*
Copyright The FleetPermit Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestSetupTracingExportsSpans checks that setting an OTLP endpoint enables
// tracing: spans reach a local OTLP/HTTP receiver when tracing shuts down.
func TestSetupTracingExportsSpans(t *testing.T) {
	var exports atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			exports.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	prevProvider, prevPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})

	shutdown, err := setupTracing(context.Background())
	if err != nil {
		t.Fatalf("tracing could not be enabled: %v", err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("tracer provider is %T, want the SDK provider", otel.GetTracerProvider())
	}
	_, span := otel.Tracer("test").Start(context.Background(), "reconcile")
	span.End()
	shutdown()
	if exports.Load() == 0 {
		t.Fatal("no spans were exported to the OTLP endpoint")
	}
}

func TestSetupTracingIsANoOpWithoutAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	prev := otel.GetTracerProvider()
	shutdown, err := setupTracing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	shutdown()
	if otel.GetTracerProvider() != prev {
		t.Fatal("tracing must stay disabled without an OTLP endpoint")
	}
}
