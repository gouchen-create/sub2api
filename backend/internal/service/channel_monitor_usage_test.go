//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ==================== 流式解析 ====================

func TestMonitorRequestBodyStreams(t *testing.T) {
	require.True(t, monitorRequestBodyStreams([]byte("{\"stream\":true,\"model\":\"m\"}")))
	require.False(t, monitorRequestBodyStreams([]byte("{\"stream\":false}")))
	require.False(t, monitorRequestBodyStreams([]byte("{\"model\":\"m\"}")))
	require.False(t, monitorRequestBodyStreams([]byte("not-json")))
}

func TestMonitorStreamContent(t *testing.T) {
	chat, done := monitorStreamContent(monitorStreamChat, "{\"choices\":[{\"delta\":{\"content\":\"7\"}}]}")
	require.Equal(t, "7", chat)
	require.False(t, done)

	delta, done := monitorStreamContent(monitorStreamResponses, "{\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}")
	require.Equal(t, "ok", delta)
	require.False(t, done)

	_, done = monitorStreamContent(monitorStreamResponses, "{\"type\":\"response.completed\"}")
	require.True(t, done)

	empty, done := monitorStreamContent(monitorStreamChat, "{\"choices\":[]}")
	require.Empty(t, empty)
	require.False(t, done)
}

func TestReadMonitorSSEResponses(t *testing.T) {
	body := strings.Join([]string{
		"event: response.output_text.delta",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}",
		"",
		"data: {\"type\":\"response.completed\",\"response\":{}}",
		"",
	}, "\n")

	text, firstTokenMs, raw := readMonitorSSE(strings.NewReader(body), monitorStreamResponses, time.Now())
	require.Equal(t, "ok", text)
	require.NotNil(t, firstTokenMs, "首个内容块到达就该记下首字")
	require.Contains(t, raw, "response.completed")
}

func TestReadMonitorSSEChat(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"1\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"2\"}}]}\n\n" +
		"data: [DONE]\n\n"

	text, firstTokenMs, _ := readMonitorSSE(strings.NewReader(body), monitorStreamChat, time.Now())
	require.Equal(t, "12", text)
	require.NotNil(t, firstTokenMs)
}

func TestMonitorUpstreamRequestID(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Oneapi-Request-Id", "req-1")
	opts := &CheckOptions{UpstreamRequestIDHeader: "X-Oneapi-Request-Id"}
	require.Equal(t, "req-1", monitorUpstreamRequestID(opts, headers))

	// 没配头名 / 没响应头 / nil opts 都返回空串。
	require.Empty(t, monitorUpstreamRequestID(&CheckOptions{}, headers))
	require.Empty(t, monitorUpstreamRequestID(opts, http.Header{}))
	require.Empty(t, monitorUpstreamRequestID(nil, headers))

	// 超长 ID 必须被裁到列宽（usage_logs.upstream_request_id VARCHAR(128)）。
	headers.Set("X-Oneapi-Request-Id", strings.Repeat("a", 400))
	got := monitorUpstreamRequestID(opts, headers)
	require.Len(t, got, maxUsageUpstreamRequestIDLen)
}

// TestCallProviderStreamReadsFirstToken 覆盖「Body 里 stream:true → 走 SSE → 记首字」的整条链路。
func TestCallProviderStreamReadsFirstToken(t *testing.T) {
	swapMonitorHTTPClient(t)
	sse := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"7\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Oneapi-Request-Id", "req-stream-1")
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(server.Close)

	opts := &CheckOptions{
		APIMode:                 MonitorAPIModeResponses,
		BodyOverrideMode:        MonitorBodyOverrideModeReplace,
		BodyOverride:            map[string]any{"model": "gpt-6-astra", "instructions": "x", "input": "hi", "stream": true},
		UpstreamRequestIDHeader: "X-Oneapi-Request-Id",
	}
	call, err := callProvider(context.Background(), MonitorProviderOpenAI, server.URL, "k", "gpt-6-astra", "hi", opts)
	require.NoError(t, err)
	require.True(t, call.Stream)
	require.Equal(t, http.StatusOK, call.Status)
	require.Equal(t, "7", call.Text)
	require.NotNil(t, call.FirstTokenMs)
	require.Equal(t, "req-stream-1", monitorUpstreamRequestID(opts, call.Headers))

	// 同样的 Body 换成 stream:false（默认 body 形态）时必须走整包读取，不产生首字。
	opts.BodyOverride["stream"] = false
	call, err = callProvider(context.Background(), MonitorProviderOpenAI, server.URL, "k", "gpt-6-astra", "hi", opts)
	require.NoError(t, err)
	require.False(t, call.Stream)
	require.Nil(t, call.FirstTokenMs)
}

// ==================== 记账 ====================

type monitorUsageAPIKeyRepoStub struct {
	APIKeyRepository
	local    *APIKey
	localErr error
	existing []APIKey
	created  *APIKey
	nextID   int64
}

func (s *monitorUsageAPIKeyRepoStub) GetByKey(context.Context, string) (*APIKey, error) {
	return s.local, s.localErr
}

func (s *monitorUsageAPIKeyRepoStub) SearchAPIKeys(context.Context, int64, string, int) ([]APIKey, error) {
	return s.existing, nil
}

func (s *monitorUsageAPIKeyRepoStub) Create(_ context.Context, key *APIKey) error {
	if s.nextID == 0 {
		s.nextID = 900
	}
	key.ID = s.nextID
	s.nextID++
	s.created = key
	return nil
}

type monitorUsageAccountRepoStub struct {
	AccountRepository
	accounts []Account
	err      error
}

func (s *monitorUsageAccountRepoStub) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	return s.accounts, s.err
}

func newMonitorUsageRecorder(
	logs *intelligenceCheckUsageLogRepoStub,
	keys *monitorUsageAPIKeyRepoStub,
	accounts *monitorUsageAccountRepoStub,
) *ChannelMonitorUsageRecorder {
	users := &intelligenceCheckUsageUserRepoStub{admin: &User{ID: 1}}
	return NewChannelMonitorUsageRecorder(logs, users, keys, accounts)
}

func monitorUsageTestMonitor() *ChannelMonitor {
	groupID := int64(37)
	return &ChannelMonitor{ID: 13, GroupID: &groupID, APIKey: "a6-upstream-key", PrimaryModel: "gpt-6-astra"}
}

func TestChannelMonitorUsagePrepareRecordsExternalProbe(t *testing.T) {
	keys := &monitorUsageAPIKeyRepoStub{localErr: ErrAPIKeyNotFound}
	accounts := &monitorUsageAccountRepoStub{accounts: []Account{{
		ID:    48,
		Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Oneapi-Request-Id"},
	}}}
	rec := newMonitorUsageRecorder(&intelligenceCheckUsageLogRepoStub{}, keys, accounts)

	probe := rec.prepareProbeUsage(context.Background(), monitorUsageTestMonitor())
	require.NotNil(t, probe)
	require.Equal(t, int64(48), probe.accountID)
	require.Equal(t, "X-Oneapi-Request-Id", probe.requestIDHeader)
}

func TestChannelMonitorUsagePrepareSkipsLocalProbe(t *testing.T) {
	// 探针 Key 能在本站 api_keys 里查到 → 走的是本地网关，网关已经记过账。
	keys := &monitorUsageAPIKeyRepoStub{local: &APIKey{ID: 7, UserID: 1}}
	accounts := &monitorUsageAccountRepoStub{accounts: []Account{{ID: 48}}}
	rec := newMonitorUsageRecorder(&intelligenceCheckUsageLogRepoStub{}, keys, accounts)

	require.Nil(t, rec.prepareProbeUsage(context.Background(), monitorUsageTestMonitor()))
}

func TestChannelMonitorUsagePrepareSkipsWithoutGroup(t *testing.T) {
	keys := &monitorUsageAPIKeyRepoStub{localErr: ErrAPIKeyNotFound}
	accounts := &monitorUsageAccountRepoStub{accounts: []Account{{ID: 48}}}
	rec := newMonitorUsageRecorder(&intelligenceCheckUsageLogRepoStub{}, keys, accounts)

	m := monitorUsageTestMonitor()
	m.GroupID = nil
	require.Nil(t, rec.prepareProbeUsage(context.Background(), m))

	empty := int64(0)
	m.GroupID = &empty
	require.Nil(t, rec.prepareProbeUsage(context.Background(), m))
}

func TestChannelMonitorUsageRecordWritesRow(t *testing.T) {
	logs := &intelligenceCheckUsageLogRepoStub{}
	keys := &monitorUsageAPIKeyRepoStub{
		localErr: ErrAPIKeyNotFound,
		existing: []APIKey{{ID: 302, UserID: 1, Name: channelMonitorUsageKeyName}},
	}
	accounts := &monitorUsageAccountRepoStub{accounts: []Account{{ID: 48}}}
	rec := newMonitorUsageRecorder(logs, keys, accounts)
	m := monitorUsageTestMonitor()
	probe := rec.prepareProbeUsage(context.Background(), m)
	require.NotNil(t, probe)

	latency := 2500
	firstToken := 321
	results := []*CheckResult{{
		Model:             "gpt-6-astra",
		Status:            MonitorStatusOperational,
		StatusCode:        200,
		Stream:            true,
		FirstTokenMs:      &firstToken,
		LatencyMs:         &latency,
		UpstreamRequestID: "req-1",
		CheckedAt:         time.Now(),
	}}

	rec.record(context.Background(), m, results, probe)

	require.Len(t, logs.logs, 1)
	log := logs.logs[0]
	require.Equal(t, int64(1), log.UserID)
	require.Equal(t, int64(302), log.APIKeyID)
	require.Equal(t, int64(48), log.AccountID)
	require.NotNil(t, log.GroupID)
	require.Equal(t, int64(37), *log.GroupID)
	require.Equal(t, "gpt-6-astra", log.Model)
	require.Equal(t, ChannelMonitorUsageInboundEndpoint, *log.InboundEndpoint)
	require.Equal(t, RequestTypeStream, log.RequestType)
	require.True(t, log.Stream)
	require.Equal(t, 2500, *log.DurationMs)
	require.Equal(t, 321, *log.FirstTokenMs)
	require.Equal(t, "req-1", *log.UpstreamRequestID)
	// 纯成本：没有向任何人收费，成本由 A6 反查回填。
	require.Zero(t, log.TotalCost)
	require.Zero(t, log.ActualCost)
}

func TestChannelMonitorUsageRecordSkipsNon2xx(t *testing.T) {
	logs := &intelligenceCheckUsageLogRepoStub{}
	keys := &monitorUsageAPIKeyRepoStub{
		localErr: ErrAPIKeyNotFound,
		existing: []APIKey{{ID: 302, UserID: 1, Name: channelMonitorUsageKeyName}},
	}
	rec := newMonitorUsageRecorder(logs, keys, &monitorUsageAccountRepoStub{accounts: []Account{{ID: 48}}})
	m := monitorUsageTestMonitor()
	probe := rec.prepareProbeUsage(context.Background(), m)
	require.NotNil(t, probe)

	results := []*CheckResult{
		{Model: "gpt-6-astra", Status: MonitorStatusError, StatusCode: 500},
		{Model: "gpt-6-astra", Status: MonitorStatusError, StatusCode: 0},
	}
	rec.record(context.Background(), m, results, probe)
	require.Empty(t, logs.logs, "非 2xx / 没拿到响应不该记消费")
}

func TestChannelMonitorUsageRecordNoopWithoutProbe(t *testing.T) {
	logs := &intelligenceCheckUsageLogRepoStub{}
	rec := newMonitorUsageRecorder(logs, &monitorUsageAPIKeyRepoStub{}, &monitorUsageAccountRepoStub{})
	results := []*CheckResult{{Model: "m", StatusCode: 200}}
	require.NotPanics(t, func() {
		rec.record(context.Background(), monitorUsageTestMonitor(), results, nil)
	})
	require.Empty(t, logs.logs)
}
