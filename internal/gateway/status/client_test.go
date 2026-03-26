package status

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFetchParsesLiveStatusSnapshot(t *testing.T) {
	client := NewClient(&http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"state":"ONLINE","inference_status":{"last_params_hash":"abc123"}}`), nil
		}),
	}, nil)

	snapshot, statusCode, err := client.Fetch(context.Background(), "http://example.test/status")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want 200", statusCode)
	}
	if snapshot == nil || snapshot.InferenceStatus.LastParamsHash != "abc123" {
		t.Fatalf("snapshot hash = %+v, want abc123", snapshot)
	}
}

func TestWaitReadyReturnsBusyErrorWhenGatewayReportsCapacityFailure(t *testing.T) {
	client := NewClient(&http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"gateway_status":{"error":{"error_message":"OrchestratorCapped"}}}`), nil
		}),
	}, nil)

	_, _, err := client.WaitReady(context.Background(), "http://example.test/status", time.Second, 10*time.Millisecond)
	if err != ErrOrchestratorBusy {
		t.Fatalf("WaitReady() error = %v, want %v", err, ErrOrchestratorBusy)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}
