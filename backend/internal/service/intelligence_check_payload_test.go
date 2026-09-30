//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newIntelligenceCheckTestContext 造一个仅用于承载覆盖值的 gin.Context。
// 官方 payload 构造函数本身不需要真实请求，覆盖值也只从 context 上取。
func newIntelligenceCheckTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Request, _ = http.NewRequest(http.MethodPost, "/", nil)
	return c
}

// 思考等级必须真的落到上游请求体里，否则这个配置就是个死设置——
// 界面上能改、数据库里有值、记录表里也有值，唯独上游收不到。
// 三家协议的字段名完全不同，写错哪一个都会表现为「配了没效果」。
func TestIntelligenceCheckPayload_InjectsReasoningEffortPerProtocol(t *testing.T) {
	t.Run("claude 用 thinking.budget_tokens", func(t *testing.T) {
		c := newIntelligenceCheckTestContext()
		withIntelligenceCheckOverride(c, intelligenceCheckOverride{
			Prompt:          "draw a pelican",
			MaxTokens:       32000,
			ReasoningEffort: "high",
		})

		payload, err := createClaudeTestPayloadForIntelligenceCheck(c, "claude-sonnet-4-5")
		require.NoError(t, err)
		require.Equal(t, 32000, payload["max_tokens"])

		thinking, ok := payload["thinking"].(map[string]any)
		require.True(t, ok, "claude payload 必须带 thinking 配置")
		require.Equal(t, "enabled", thinking["type"])
		require.Equal(t, 16384, thinking["budget_tokens"])
	})

	t.Run("claude 的思考预算必须严格小于输出上限", func(t *testing.T) {
		c := newIntelligenceCheckTestContext()
		// 输出上限只比最小预算大一点：此时不能硬塞一个更大的预算，
		// 否则上游会因为 budget_tokens >= max_tokens 直接拒绝请求。
		withIntelligenceCheckOverride(c, intelligenceCheckOverride{
			Prompt:          "draw a pelican",
			MaxTokens:       6000,
			ReasoningEffort: "max",
		})

		payload, err := createClaudeTestPayloadForIntelligenceCheck(c, "claude-sonnet-4-5")
		require.NoError(t, err)

		thinking, ok := payload["thinking"].(map[string]any)
		require.True(t, ok)
		budget, ok := thinking["budget_tokens"].(int)
		require.True(t, ok)
		require.Less(t, budget, 6000, "预算必须小于 max_tokens")
		require.GreaterOrEqual(t, budget, claudeThinkingMinBudgetTokens)
	})

	t.Run("claude 输出上限太小时不注入思考配置", func(t *testing.T) {
		c := newIntelligenceCheckTestContext()
		withIntelligenceCheckOverride(c, intelligenceCheckOverride{
			Prompt:          "draw a pelican",
			MaxTokens:       2048,
			ReasoningEffort: "high",
		})

		payload, err := createClaudeTestPayloadForIntelligenceCheck(c, "claude-sonnet-4-5")
		require.NoError(t, err)
		_, hasThinking := payload["thinking"]
		require.False(t, hasThinking, "塞不进合法预算时宁可走上游默认，也不要发非法请求")
	})

	t.Run("openai responses 用嵌套 reasoning.effort", func(t *testing.T) {
		c := newIntelligenceCheckTestContext()
		withIntelligenceCheckOverride(c, intelligenceCheckOverride{
			Prompt:          "draw a pelican",
			MaxTokens:       32000,
			ReasoningEffort: "xhigh",
		})

		payload := createOpenAIResponsesPayloadForIntelligenceCheck(c, "gpt-6-astra", false)
		reasoning, ok := payload["reasoning"].(map[string]any)
		require.True(t, ok, "responses payload 必须带嵌套 reasoning")
		require.Equal(t, "xhigh", reasoning["effort"])
		require.Equal(t, 32000, payload["max_output_tokens"])
	})

	t.Run("openai chat 用顶层 reasoning_effort", func(t *testing.T) {
		c := newIntelligenceCheckTestContext()
		withIntelligenceCheckOverride(c, intelligenceCheckOverride{
			Prompt:          "draw a pelican",
			MaxTokens:       32000,
			ReasoningEffort: "medium",
		})

		payload := createOpenAIChatPayloadForIntelligenceCheck(c, "deepseek-v4.1-flash", "draw a pelican")
		require.Equal(t, "medium", payload["reasoning_effort"])
		require.Equal(t, 32000, payload["max_tokens"])
	})
}

// 留空＝不干预：绝不能凭空塞一个上游不认的字段，把官方连通性测试的请求体也一起改了。
func TestIntelligenceCheckPayload_EmptyEffortKeepsOfficialShape(t *testing.T) {
	c := newIntelligenceCheckTestContext()
	withIntelligenceCheckOverride(c, intelligenceCheckOverride{
		Prompt:    "draw a pelican",
		MaxTokens: 32000,
	})

	claudePayload, err := createClaudeTestPayloadForIntelligenceCheck(c, "claude-sonnet-4-5")
	require.NoError(t, err)
	_, hasThinking := claudePayload["thinking"]
	require.False(t, hasThinking)

	responsesPayload := createOpenAIResponsesPayloadForIntelligenceCheck(c, "gpt-6-astra", false)
	_, hasReasoning := responsesPayload["reasoning"]
	require.False(t, hasReasoning)

	chatPayload := createOpenAIChatPayloadForIntelligenceCheck(c, "deepseek-v4.1-flash", "draw a pelican")
	_, hasEffort := chatPayload["reasoning_effort"]
	require.False(t, hasEffort)
}

// 没有覆盖值时，包装函数必须与官方原函数逐字节等价——
// 官方账号连通性测试、定时连通性测试、用量采样都共用同一条构造链路。
func TestIntelligenceCheckPayload_WithoutOverrideMatchesOfficial(t *testing.T) {
	c := newIntelligenceCheckTestContext()

	wrapped, err := createClaudeTestPayloadForIntelligenceCheck(c, "claude-sonnet-4-5")
	require.NoError(t, err)
	official, err := createTestPayload("claude-sonnet-4-5")
	require.NoError(t, err)

	// sessionID 每次都不同，比较时排除 metadata。
	delete(wrapped, "metadata")
	delete(official, "metadata")
	require.Equal(t, official, wrapped)

	require.Equal(t, createOpenAITestPayload("gpt-6-astra", false),
		createOpenAIResponsesPayloadForIntelligenceCheck(c, "gpt-6-astra", false))

	require.Equal(t, createOpenAIChatCompletionsTestPayload("deepseek-v4.1-flash", "hi"),
		createOpenAIChatPayloadForIntelligenceCheck(c, "deepseek-v4.1-flash", "hi"))
}

// 账号级覆盖优先、留空回落全局——两级回落必须两个入口都实现，
// 否则会出现「手动重跑生效、定时任务不生效」这种只在特定路径失效的诡异现象。
func TestReadIntelligenceCheckAccountConfig_ReasoningEffort(t *testing.T) {
	t.Run("读到账号级覆盖", func(t *testing.T) {
		config := ReadIntelligenceCheckAccountConfig(map[string]any{
			IntelligenceCheckEnabledExtraKey:         true,
			IntelligenceCheckReasoningEffortExtraKey: "low",
		})
		require.Equal(t, "low", config.ReasoningEffort)
	})

	t.Run("缺省时为空即跟随全局", func(t *testing.T) {
		config := ReadIntelligenceCheckAccountConfig(map[string]any{
			IntelligenceCheckEnabledExtraKey: true,
		})
		require.Equal(t, "", config.ReasoningEffort)
	})

	t.Run("类型不符时回退成未覆盖", func(t *testing.T) {
		config := ReadIntelligenceCheckAccountConfig(map[string]any{
			IntelligenceCheckReasoningEffortExtraKey: 42,
		})
		require.Equal(t, "", config.ReasoningEffort)
	})
}

// 留空即「跟随全局」，所以归一化要把空值对应的键删掉，
// 而不是留一个空字符串在 extra 里制造「配过了」的假象。
func TestNormalizeIntelligenceCheckExtra_ReasoningEffort(t *testing.T) {
	t.Run("空串删除键", func(t *testing.T) {
		normalized, err := NormalizeIntelligenceCheckExtra(map[string]any{
			IntelligenceCheckReasoningEffortExtraKey: "   ",
		})
		require.NoError(t, err)
		_, exists := normalized[IntelligenceCheckReasoningEffortExtraKey]
		require.False(t, exists)
	})

	t.Run("非空值去除首尾空白后保留", func(t *testing.T) {
		normalized, err := NormalizeIntelligenceCheckExtra(map[string]any{
			IntelligenceCheckReasoningEffortExtraKey: "  high  ",
		})
		require.NoError(t, err)
		require.Equal(t, "high", normalized[IntelligenceCheckReasoningEffortExtraKey])
	})

	t.Run("类型不符直接拒绝", func(t *testing.T) {
		_, err := NormalizeIntelligenceCheckExtra(map[string]any{
			IntelligenceCheckReasoningEffortExtraKey: 123,
		})
		require.Error(t, err)
	})
}
