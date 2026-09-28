package service

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIUpstreamIdentityPreventsGzipBufferedSSE(t *testing.T) {
	type readResult struct {
		line string
		err  error
	}

	run := func(t *testing.T, acceptEncoding string) bool {
		t.Helper()

		preambleWritten := make(chan struct{})
		releaseTerminal := make(chan struct{})
		seenEncoding := make(chan string, 1)

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			encoding := r.Header.Get("Accept-Encoding")
			seenEncoding <- encoding
			w.Header().Set("Content-Type", "text/event-stream")

			var dst io.Writer = w
			var gz *gzip.Writer
			if strings.Contains(encoding, "gzip") {
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				gz = gzip.NewWriter(w)
				dst = gz
			}

			_, _ = fmt.Fprint(dst, "data: {\"type\":\"response.created\"}\n\n")
			if gz == nil {
				w.(http.Flusher).Flush()
			}
			close(preambleWritten)

			<-releaseTerminal
			_, _ = fmt.Fprint(dst, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
			_, _ = fmt.Fprint(dst, "data: {\"type\":\"response.completed\"}\n\n")
			if gz != nil {
				_ = gz.Close()
			} else {
				w.(http.Flusher).Flush()
			}
		}))
		defer upstream.Close()

		req, err := http.NewRequest(http.MethodPost, upstream.URL, nil)
		require.NoError(t, err)
		if acceptEncoding != "" {
			req.Header.Set("Accept-Encoding", acceptEncoding)
		}

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)
		firstLine := make(chan readResult, 1)
		go func() {
			line, readErr := reader.ReadString('\n')
			firstLine <- readResult{line: line, err: readErr}
		}()

		<-preambleWritten
		var beforeRelease bool
		var result readResult
		select {
		case result = <-firstLine:
			beforeRelease = true
		case <-time.After(100 * time.Millisecond):
		}

		close(releaseTerminal)
		if !beforeRelease {
			result = <-firstLine
		}
		require.NoError(t, result.err)
		require.Contains(t, result.line, "response.created")

		observed := <-seenEncoding
		if acceptEncoding == "" {
			require.Contains(t, observed, "gzip")
		} else {
			require.Equal(t, acceptEncoding, observed)
		}
		return beforeRelease
	}

	t.Run("default_transport_buffers_gzip_stream", func(t *testing.T) {
		require.False(t, run(t, ""))
	})

	t.Run("identity_streams_preamble_immediately", func(t *testing.T) {
		require.True(t, run(t, "identity"))
	})
}

func TestOpenAIRequestBuildersForceIdentityEncoding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Accept-Encoding", "gzip")

	svc := &OpenAIGatewayService{cfg: &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://example.com/v1",
		},
	}
	body := []byte("{\"model\":\"gpt-5.2\",\"stream\":true}")

	regular, err := svc.buildUpstreamRequest(t.Context(), c, account, body, "token", true, "", false)
	require.NoError(t, err)
	require.Equal(t, "identity", regular.Header.Get("Accept-Encoding"))

	passthrough, err := svc.buildUpstreamRequestOpenAIPassthrough(t.Context(), c, account, body, "token")
	require.NoError(t, err)
	require.Equal(t, "identity", passthrough.Header.Get("Accept-Encoding"))

	nonStreamBody := []byte("{\"model\":\"gpt-5.2\",\"stream\":false}")
	nonStream, err := svc.buildUpstreamRequest(t.Context(), c, account, nonStreamBody, "token", false, "", false)
	require.NoError(t, err)
	require.Empty(t, nonStream.Header.Get("Accept-Encoding"), "non-streaming requests must keep normal Transport compression negotiation")

	nonStreamPassthrough, err := svc.buildUpstreamRequestOpenAIPassthrough(t.Context(), c, account, nonStreamBody, "token")
	require.NoError(t, err)
	require.Empty(t, nonStreamPassthrough.Header.Get("Accept-Encoding"), "non-streaming passthrough must keep normal Transport compression negotiation")
}

func TestOpenAIResponsesPreambleWaitsForFirstClientOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{"X-Request-Id": []string{"rid-timing"}},
	}
	start := time.Now()
	type result struct {
		value *openaiStreamingResult
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, start, "model", "model")
		done <- result{value: value, err: err}
	}()

	_, err := pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_timing\"}}\n\n"))
	require.NoError(t, err)
	time.Sleep(40 * time.Millisecond)
	require.Empty(t, rec.Body.String(), "preamble must remain buffered before client-visible output")

	_, err = pw.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
	require.NoError(t, err)
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(rec.Body.String(), "response.output_text.delta") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Contains(t, rec.Body.String(), "response.created")
	require.Contains(t, rec.Body.String(), "response.output_text.delta")

	_, err = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_timing\",\"status\":\"completed\"}}\n\n"))
	require.NoError(t, err)
	require.NoError(t, pw.Close())

	completed := <-done
	require.NoError(t, completed.err)
	require.NotNil(t, completed.value)
	require.NotNil(t, completed.value.firstTokenMs)
	require.GreaterOrEqual(t, *completed.value.firstTokenMs, 20)
}
