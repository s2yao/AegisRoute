package httperror_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/observability"
)

func TestWriteExactShapeWithRequestID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = req.WithContext(observability.ContextWithRequestID(req.Context(), "req-123"))
	rec := httptest.NewRecorder()

	httperror.Write(rec, req, http.StatusNotFound, httperror.CodeNotFound, "no such model")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.JSONEq(t,
		`{"error":{"code":"not_found","message":"no such model","request_id":"req-123"}}`,
		rec.Body.String())
}

func TestWriteWithoutRequestID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	httperror.Write(rec, req, http.StatusInternalServerError, httperror.CodeInternal, "boom")

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t,
		`{"error":{"code":"internal","message":"boom","request_id":""}}`,
		rec.Body.String())
}

func TestWriteStatusPerCode(t *testing.T) {
	cases := []struct {
		status int
		code   string
	}{
		{http.StatusUnauthorized, httperror.CodeUnauthorized},
		{http.StatusBadRequest, httperror.CodeBadRequest},
		{http.StatusConflict, httperror.CodeConflict},
		{http.StatusTooManyRequests, httperror.CodeRateLimited},
		{http.StatusBadGateway, httperror.CodeUpstreamUnavailable},
		{http.StatusBadRequest, httperror.CodeUnsupportedStreaming},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(observability.ContextWithRequestID(req.Context(), "rid"))
		rec := httptest.NewRecorder()

		httperror.Write(rec, req, tc.status, tc.code, "msg")

		assert.Equal(t, tc.status, rec.Code, tc.code)
		assert.Contains(t, rec.Body.String(), `"code":"`+tc.code+`"`)
		assert.Contains(t, rec.Body.String(), `"request_id":"rid"`)
	}
}
