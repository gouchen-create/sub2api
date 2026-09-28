//go:build unit

package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// companionEnvelope 是本项目统一响应信封，用于断言代理返回。
type companionEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func companionTestRouter(handler *CompanionHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/status", handler.Status)
	router.GET("/summary", handler.Summary)
	router.PUT("/account-rules/:account_id", handler.UpsertAccountRule)
	router.POST("/collect", handler.Collect)
	return router
}

func companionDo(t *testing.T, router *gin.Engine, method, path, body string) (int, companionEnvelope) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	var envelope companionEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, recorder.Body.String())
	}
	return recorder.Code, envelope
}

// TestCompanionStatusDisabledWithoutBaseURL 未配置上游地址时必须给出明确的关闭态，
// 让管理后台页面能展示配置引导而不是报错。
func TestCompanionStatusDisabledWithoutBaseURL(t *testing.T) {
	t.Setenv(envCompanionBaseURL, "")
	handler := NewCompanionHandler()
	if handler.Enabled() {
		t.Fatal("未配置 COMPANION_BASE_URL 时 Enabled() 必须为 false")
	}

	status, envelope := companionDo(t, companionTestRouter(handler), http.MethodGet, "/status", "")
	if status != http.StatusOK {
		t.Fatalf("status 期望 200，实际 %d", status)
	}
	var payload struct {
		Enabled bool `json:"enabled"`
		Healthy bool `json:"healthy"`
	}
	if err := json.Unmarshal(envelope.Data, &payload); err != nil {
		t.Fatalf("解析 status 数据失败: %v", err)
	}
	if payload.Enabled || payload.Healthy {
		t.Fatalf("未配置时 enabled/healthy 必须为 false，实际 %+v", payload)
	}

	_, relayed := companionDo(t, companionTestRouter(handler), http.MethodGet, "/summary", "")
	if relayed.Code == 0 || !strings.Contains(relayed.Message, "COMPANION_NOT_CONFIGURED") {
		t.Fatalf("业务接口应返回 COMPANION_NOT_CONFIGURED，实际 code=%d message=%s", relayed.Code, relayed.Message)
	}
}

// TestCompanionRelaysUpstreamJSON 验证正向代理：附带 Basic 凭据、透传查询串与请求体，
// 并把上游 JSON 放进统一信封的 data 字段。
func TestCompanionRelaysUpstreamJSON(t *testing.T) {
	var (
		gotUser   string
		gotPass   string
		gotOK     bool
		gotQuery  string
		gotBody   string
		gotMethod string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		gotQuery = r.URL.RawQuery
		gotMethod = r.Method
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)

		switch r.URL.Path {
		case "/ops/api/summary":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total_cost_cny":12.5,"requests":7}`))
		case "/ops/api/account-rules/42":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"account_id":42,"provider":"a6","multiplier":1.2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	t.Setenv(envCompanionBaseURL, upstream.URL)
	t.Setenv(envCompanionAdminUser, "panel-user")
	t.Setenv(envCompanionAdminPass, "panel-pass")
	handler := NewCompanionHandler()
	if !handler.Enabled() {
		t.Fatal("配置了 COMPANION_BASE_URL 后 Enabled() 必须为 true")
	}
	router := companionTestRouter(handler)

	status, envelope := companionDo(t, router, http.MethodGet, "/summary?from=2026-09-01&to=2026-09-28", "")
	if status != http.StatusOK || envelope.Code != 0 {
		t.Fatalf("summary 期望 200/code=0，实际 %d/%d message=%s", status, envelope.Code, envelope.Message)
	}
	if !strings.Contains(string(envelope.Data), "total_cost_cny") {
		t.Fatalf("上游 JSON 未被透传: %s", string(envelope.Data))
	}
	if !gotOK || gotUser != "panel-user" || gotPass != "panel-pass" {
		t.Fatalf("上游 Basic 凭据不正确: ok=%v user=%q", gotOK, gotUser)
	}
	if gotQuery != "from=2026-09-01&to=2026-09-28" {
		t.Fatalf("查询串未透传，上游收到 %q", gotQuery)
	}

	status, envelope = companionDo(t, router, http.MethodPut, "/account-rules/42", `{"provider":"a6","multiplier":1.2}`)
	if status != http.StatusOK || envelope.Code != 0 {
		t.Fatalf("account-rules 期望 200/code=0，实际 %d/%d message=%s", status, envelope.Code, envelope.Message)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("上游方法期望 PUT，实际 %s", gotMethod)
	}
	if !strings.Contains(gotBody, "multiplier") {
		t.Fatalf("请求体未透传，上游收到 %q", gotBody)
	}
}

// TestCompanionUpstreamAuthFailureDoesNotLeak401 上游鉴权失败必须映射为 502，
// 否则面板前端会把 401 当成管理员会话失效并把用户踢到登录页。
func TestCompanionUpstreamAuthFailureDoesNotLeak401(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer upstream.Close()

	t.Setenv(envCompanionBaseURL, upstream.URL)
	t.Setenv(envCompanionAdminUser, "wrong")
	t.Setenv(envCompanionAdminPass, "wrong")
	handler := NewCompanionHandler()

	status, envelope := companionDo(t, companionTestRouter(handler), http.MethodGet, "/summary", "")
	if status != http.StatusBadGateway {
		t.Fatalf("上游 401 应映射为 502，实际 %d", status)
	}
	if !strings.Contains(envelope.Message, "COMPANION_AUTH_FAILED") {
		t.Fatalf("错误信息应提示凭据不一致，实际 %s", envelope.Message)
	}
}

// TestCompanionRejectsNonNumericAccountID 账号 ID 必须是数字，避免被拼进上游 URL。
func TestCompanionRejectsNonNumericAccountID(t *testing.T) {
	t.Setenv(envCompanionBaseURL, "http://127.0.0.1:1")
	handler := NewCompanionHandler()

	status, envelope := companionDo(t, companionTestRouter(handler), http.MethodPut, "/account-rules/abc", `{}`)
	if status != http.StatusBadRequest {
		t.Fatalf("非法 account_id 期望 400，实际 %d", status)
	}
	if !strings.Contains(envelope.Message, "account_id") {
		t.Fatalf("错误信息应说明 account_id 非法，实际 %s", envelope.Message)
	}
}

// TestCompanionUnreachableMapsToBadGateway 上游不可达时给出可诊断的错误码。
func TestCompanionUnreachableMapsToBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL := upstream.URL
	upstream.Close() // 关闭后端口不可达

	t.Setenv(envCompanionBaseURL, upstreamURL)
	handler := NewCompanionHandler()

	status, envelope := companionDo(t, companionTestRouter(handler), http.MethodGet, "/summary", "")
	if status != http.StatusBadGateway {
		t.Fatalf("上游不可达期望 502，实际 %d", status)
	}
	if !strings.Contains(envelope.Message, "COMPANION_UNREACHABLE") {
		t.Fatalf("错误信息应含 COMPANION_UNREACHABLE，实际 %s", envelope.Message)
	}
}
