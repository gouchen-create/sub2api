package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

const (
	providerPricingSchemaVersion = "1.1"
	providerPricingCurrencyCNY   = "CNY"

	providerPricingUnitPer1MTokens = "per_1m_tokens"
	providerPricingUnitPerCall     = "per_call"

	providerPricingTokensPerMillion = 1000000.0
)

type providerPricingChannelService interface {
	ListAvailable(ctx context.Context) ([]service.AvailableChannel, error)
}

type ProviderPricingHandler struct {
	channelService providerPricingChannelService
	now            func() time.Time
}

type providerPricingResponse struct {
	SchemaVersion string              `json:"schema_version"`
	Success       bool                `json:"success"`
	Message       string              `json:"message,omitempty"`
	Data          providerPricingData `json:"data"`
}

type providerPricingData struct {
	Currency  string                 `json:"currency"`
	PriceUnit string                 `json:"price_unit"`
	UpdatedAt string                 `json:"updated_at"`
	Models    []providerPricingModel `json:"models"`
}

type providerPricingModel struct {
	ModelName          string                   `json:"model_name"`
	GroupName          string                   `json:"group_name"`
	ChannelName        string                   `json:"channel_name"`
	Platform           string                   `json:"platform"`
	BillingMode        string                   `json:"billing_mode"`
	PriceUnit          string                   `json:"price_unit"`
	InputPrice         *float64                 `json:"input_price,omitempty"`
	OutputPrice        *float64                 `json:"output_price,omitempty"`
	CacheCreatePrice   *float64                 `json:"cache_create_price,omitempty"`
	CacheCreatePrice1h *float64                 `json:"cache_create_price_1h,omitempty"`
	CacheInputPrice    *float64                 `json:"cache_input_price,omitempty"`
	UnitPrice          *float64                 `json:"unit_price,omitempty"`
	Enabled            bool                     `json:"enabled"`
	Note               string                   `json:"note,omitempty"`
	Tiers              []providerPricingTierDTO `json:"tiers,omitempty"`
}

type providerPricingTierDTO struct {
	MinTokens          int      `json:"min_tokens"`
	MaxTokens          *int     `json:"max_tokens,omitempty"`
	TierLabel          string   `json:"tier_label,omitempty"`
	InputPrice         *float64 `json:"input_price,omitempty"`
	OutputPrice        *float64 `json:"output_price,omitempty"`
	CacheCreatePrice   *float64 `json:"cache_create_price,omitempty"`
	CacheCreatePrice1h *float64 `json:"cache_create_price_1h,omitempty"`
	CacheInputPrice    *float64 `json:"cache_input_price,omitempty"`
	UnitPrice          *float64 `json:"unit_price,omitempty"`
}

func NewProviderPricingHandler(channelService *service.ChannelService) *ProviderPricingHandler {
	return &ProviderPricingHandler{
		channelService: channelService,
		now:            time.Now,
	}
}

// List exposes Hvoy Provider Pricing API v1.1.
func (h *ProviderPricingHandler) List(c *gin.Context) {
	if h == nil || h.channelService == nil {
		response.InternalError(c, "provider pricing service unavailable")
		return
	}

	channels, err := h.channelService.ListAvailable(c.Request.Context())
	if err != nil {
		response.InternalError(c, "failed to load provider pricing")
		return
	}

	now := time.Now
	if h.now != nil {
		now = h.now
	}

	c.JSON(http.StatusOK, providerPricingResponse{
		SchemaVersion: providerPricingSchemaVersion,
		Success:       true,
		Data: providerPricingData{
			Currency:  providerPricingCurrencyCNY,
			PriceUnit: providerPricingUnitPer1MTokens,
			UpdatedAt: now().UTC().Format(time.RFC3339),
			Models:    buildProviderPricingModels(channels),
		},
	})
}

func buildProviderPricingModels(channels []service.AvailableChannel) []providerPricingModel {
	out := make([]providerPricingModel, 0)
	for _, ch := range channels {
		if ch.Status != service.StatusActive {
			continue
		}
		groupsByPlatform := providerGroupsByPlatform(ch.Groups)
		if len(groupsByPlatform) == 0 {
			continue
		}

		for _, model := range ch.SupportedModels {
			groups := groupsByPlatform[model.Platform]
			if len(groups) == 0 {
				continue
			}
			for _, group := range groups {
				row := toProviderPricingModel(ch, group, model)
				out = append(out, row)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].GroupName != out[j].GroupName {
			return strings.ToLower(out[i].GroupName) < strings.ToLower(out[j].GroupName)
		}
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		if out[i].ModelName != out[j].ModelName {
			return strings.ToLower(out[i].ModelName) < strings.ToLower(out[j].ModelName)
		}
		return strings.ToLower(out[i].ChannelName) < strings.ToLower(out[j].ChannelName)
	})
	return out
}

func providerGroupsByPlatform(groups []service.AvailableGroupRef) map[string][]service.AvailableGroupRef {
	out := make(map[string][]service.AvailableGroupRef, len(groups))
	for _, group := range groups {
		if group.Platform == "" {
			continue
		}
		out[group.Platform] = append(out[group.Platform], group)
	}
	for platform := range out {
		sort.SliceStable(out[platform], func(i, j int) bool {
			return strings.ToLower(out[platform][i].Name) < strings.ToLower(out[platform][j].Name)
		})
	}
	return out
}

func toProviderPricingModel(ch service.AvailableChannel, group service.AvailableGroupRef, model service.SupportedModel) providerPricingModel {
	pricing := model.Pricing
	billingMode := string(service.BillingModeToken)
	if pricing != nil && pricing.BillingMode != "" {
		billingMode = string(pricing.BillingMode)
	}

	priceUnit := providerPricingUnitPer1MTokens
	if billingMode == string(service.BillingModePerRequest) || billingMode == string(service.BillingModeImage) {
		priceUnit = providerPricingUnitPerCall
	}

	row := providerPricingModel{
		ModelName:   model.Name,
		GroupName:   group.Name,
		ChannelName: ch.Name,
		Platform:    model.Platform,
		BillingMode: billingMode,
		PriceUnit:   priceUnit,
		Enabled:     true,
		Note:        providerPricingNote(ch.Description, group.RateMultiplier),
	}
	if pricing == nil {
		return row
	}

	multiplier := group.RateMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	if priceUnit == providerPricingUnitPer1MTokens {
		row.InputPrice = providerTokenPriceToCNYPerMillion(pricing.InputPrice, multiplier)
		row.OutputPrice = providerTokenPriceToCNYPerMillion(pricing.OutputPrice, multiplier)
		row.CacheCreatePrice = providerTokenPriceToCNYPerMillion(pricing.CacheWritePrice, multiplier)
		row.CacheInputPrice = providerTokenPriceToCNYPerMillion(pricing.CacheReadPrice, multiplier)
	} else {
		row.UnitPrice = providerPerCallPriceToCNY(firstNonNilFloat(pricing.PerRequestPrice, pricing.ImageOutputPrice), multiplier)
	}
	row.Tiers = toProviderPricingTiers(pricing.Intervals, priceUnit, multiplier)
	return row
}

func toProviderPricingTiers(intervals []service.PricingInterval, priceUnit string, multiplier float64) []providerPricingTierDTO {
	if len(intervals) == 0 {
		return nil
	}
	out := make([]providerPricingTierDTO, 0, len(intervals))
	for _, iv := range intervals {
		tier := providerPricingTierDTO{
			MinTokens: iv.MinTokens,
			MaxTokens: iv.MaxTokens,
			TierLabel: iv.TierLabel,
		}
		if priceUnit == providerPricingUnitPer1MTokens {
			tier.InputPrice = providerTokenPriceToCNYPerMillion(iv.InputPrice, multiplier)
			tier.OutputPrice = providerTokenPriceToCNYPerMillion(iv.OutputPrice, multiplier)
			tier.CacheCreatePrice = providerTokenPriceToCNYPerMillion(iv.CacheWritePrice, multiplier)
			tier.CacheInputPrice = providerTokenPriceToCNYPerMillion(iv.CacheReadPrice, multiplier)
		} else {
			tier.UnitPrice = providerPerCallPriceToCNY(iv.PerRequestPrice, multiplier)
		}
		out = append(out, tier)
	}
	return out
}

func providerTokenPriceToCNYPerMillion(price *float64, multiplier float64) *float64 {
	if price == nil {
		return nil
	}
	v := *price * providerPricingTokensPerMillion * multiplier
	return float64Ptr(roundProviderPrice(v))
}

func providerPerCallPriceToCNY(price *float64, multiplier float64) *float64 {
	if price == nil {
		return nil
	}
	v := *price * multiplier
	return float64Ptr(roundProviderPrice(v))
}

func roundProviderPrice(v float64) float64 {
	return math.Round(v*1e8) / 1e8
}

func firstNonNilFloat(values ...*float64) *float64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func providerPricingNote(description string, multiplier float64) string {
	parts := make([]string, 0, 2)
	if multiplier > 0 && multiplier != 1 {
		parts = append(parts, fmt.Sprintf("group multiplier %.4g applied", multiplier))
	}
	description = strings.TrimSpace(description)
	if description != "" {
		parts = append(parts, description)
	}
	return strings.Join(parts, "; ")
}

func float64Ptr(v float64) *float64 {
	return &v
}
