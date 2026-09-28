package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type providerPricingChannelServiceStub struct {
	channels []service.AvailableChannel
	err      error
}

func (s *providerPricingChannelServiceStub) ListAvailable(context.Context) ([]service.AvailableChannel, error) {
	return s.channels, s.err
}

func TestProviderPricingHandlerList_ReturnsHvoySchema(t *testing.T) {
	gin.SetMode(gin.TestMode)

	inputPrice := 2.5e-6
	outputPrice := 15e-6
	cacheReadPrice := 0.25e-6
	cacheWritePrice := 1.25e-6
	handler := &ProviderPricingHandler{
		channelService: &providerPricingChannelServiceStub{channels: []service.AvailableChannel{
			{
				Name:        "A6 OpenAI",
				Description: "primary",
				Status:      service.StatusActive,
				Groups: []service.AvailableGroupRef{
					{Name: "standard", Platform: service.PlatformOpenAI, RateMultiplier: 1.2},
					{Name: "anthropic", Platform: service.PlatformAnthropic, RateMultiplier: 1},
				},
				SupportedModels: []service.SupportedModel{
					{
						Name:     "gpt-5.5",
						Platform: service.PlatformOpenAI,
						Pricing: &service.ChannelModelPricing{
							BillingMode:     service.BillingModeToken,
							InputPrice:      &inputPrice,
							OutputPrice:     &outputPrice,
							CacheWritePrice: &cacheWritePrice,
							CacheReadPrice:  &cacheReadPrice,
						},
					},
					{
						Name:     "claude-sonnet-4.5",
						Platform: service.PlatformAnthropic,
						Pricing:  &service.ChannelModelPricing{BillingMode: service.BillingModeToken, InputPrice: &inputPrice},
					},
				},
			},
			{
				Name:   "inactive",
				Status: service.StatusDisabled,
			},
		}},
		now: func() time.Time { return time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC) },
	}

	router := gin.New()
	router.GET("/api/provider/pricing", handler.List)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/provider/pricing", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp providerPricingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.SchemaVersion != providerPricingSchemaVersion {
		t.Fatalf("schema_version = %q", resp.SchemaVersion)
	}
	if !resp.Success {
		t.Fatal("success should be true")
	}
	if resp.Data.Currency != providerPricingCurrencyCNY {
		t.Fatalf("currency = %q", resp.Data.Currency)
	}
	if resp.Data.PriceUnit != providerPricingUnitPer1MTokens {
		t.Fatalf("price_unit = %q", resp.Data.PriceUnit)
	}
	if resp.Data.UpdatedAt != "2026-07-13T01:02:03Z" {
		t.Fatalf("updated_at = %q", resp.Data.UpdatedAt)
	}
	if len(resp.Data.Models) != 2 {
		t.Fatalf("models length = %d, want 2: %#v", len(resp.Data.Models), resp.Data.Models)
	}

	var model providerPricingModel
	for _, candidate := range resp.Data.Models {
		if candidate.ModelName == "gpt-5.5" && candidate.GroupName == "standard" {
			model = candidate
			break
		}
	}
	if model.ModelName != "gpt-5.5" || model.GroupName != "standard" || model.ChannelName != "A6 OpenAI" {
		t.Fatalf("unexpected model identity: %#v", model)
	}
	if model.Platform != service.PlatformOpenAI || model.PriceUnit != providerPricingUnitPer1MTokens {
		t.Fatalf("unexpected platform/unit: %#v", model)
	}
	if !model.Enabled {
		t.Fatal("model should be enabled")
	}
	wantInput := inputPrice * providerPricingTokensPerMillion * 1.2
	if model.InputPrice == nil || *model.InputPrice != roundProviderPrice(wantInput) {
		t.Fatalf("input_price = %#v, want %.8f", model.InputPrice, roundProviderPrice(wantInput))
	}
	wantOutput := outputPrice * providerPricingTokensPerMillion * 1.2
	if model.OutputPrice == nil || *model.OutputPrice != roundProviderPrice(wantOutput) {
		t.Fatalf("output_price = %#v, want %.8f", model.OutputPrice, roundProviderPrice(wantOutput))
	}
	if model.CacheInputPrice == nil {
		t.Fatal("cache_input_price should be set")
	}
	if model.CacheCreatePrice == nil {
		t.Fatal("cache_create_price should be set")
	}
	if strings.Contains(strings.ToLower(model.Note), "usd") {
		t.Fatalf("note should not describe USD conversion: %q", model.Note)
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw response: %v", err)
	}
	data, ok := raw["data"].(map[string]any)
	if !ok {
		t.Fatalf("raw data has unexpected type: %#v", raw["data"])
	}
	models, ok := data["models"].([]any)
	if !ok || len(models) == 0 {
		t.Fatalf("raw models has unexpected value: %#v", data["models"])
	}
	firstModel, ok := models[0].(map[string]any)
	if !ok {
		t.Fatalf("raw model has unexpected type: %#v", models[0])
	}
	if _, ok := firstModel["cache_read_price"]; ok {
		t.Fatal("response must use Hvoy cache_input_price, not cache_read_price")
	}
	if _, ok := firstModel["cache_write_price"]; ok {
		t.Fatal("response must use Hvoy cache_create_price, not cache_write_price")
	}
}

func TestProviderPricingHandlerList_UsesProviderCurrencyOneToOne(t *testing.T) {
	gin.SetMode(gin.TestMode)

	perRequestPrice := 0.03
	handler := &ProviderPricingHandler{
		channelService: &providerPricingChannelServiceStub{channels: []service.AvailableChannel{
			{
				Name:   "A6 Image",
				Status: service.StatusActive,
				Groups: []service.AvailableGroupRef{
					{Name: "standard", Platform: service.PlatformOpenAI, RateMultiplier: 2},
				},
				SupportedModels: []service.SupportedModel{
					{
						Name:     "image-model",
						Platform: service.PlatformOpenAI,
						Pricing: &service.ChannelModelPricing{
							BillingMode:     service.BillingModePerRequest,
							PerRequestPrice: &perRequestPrice,
						},
					},
				},
			},
		}},
		now: func() time.Time { return time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC) },
	}

	router := gin.New()
	router.GET("/api/provider/pricing", handler.List)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/provider/pricing", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp providerPricingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Data.Models) != 1 {
		t.Fatalf("models length = %d, want 1: %#v", len(resp.Data.Models), resp.Data.Models)
	}
	model := resp.Data.Models[0]
	wantUnitPrice := perRequestPrice * 2
	if model.UnitPrice == nil || *model.UnitPrice != roundProviderPrice(wantUnitPrice) {
		t.Fatalf("unit_price = %#v, want %.8f", model.UnitPrice, roundProviderPrice(wantUnitPrice))
	}
}

func TestProviderPricingHandlerList_ServiceError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &ProviderPricingHandler{
		channelService: &providerPricingChannelServiceStub{err: errors.New("db down")},
		now:            time.Now,
	}
	router := gin.New()
	router.GET("/api/provider/pricing", handler.List)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/provider/pricing", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
