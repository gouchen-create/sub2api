package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// 内部探针记账的归属解析（智力检测、渠道监控直连探针共用）。
//
// usage_logs 的 user_id / api_key_id 是 NOT NULL 外键，而内部探针没有真实用户：
//   - user_id   统一挂到「第一个可用管理员」名下（主人指定）；
//   - api_key_id 挂一个按名字查找、必要时自动创建的 disabled 专用 Key。
//
// 专用 Key 不参与认证（disabled），也不会借用管理员正在用的某个 Key——那会让那个
// Key 的用量统计凭空变脏。删除它会级联带走这些记账行，因此名字里写了「请勿删除」。
type internalUsageAttribution struct {
	UserID   int64
	APIKeyID int64
}

// resolveInternalUsageAttribution 解析（或创建）一份内部记账归属。
// keyName 是专用 Key 的名字，各功能用自己的名字以便在 Key 列表里一眼分辨。
func resolveInternalUsageAttribution(
	ctx context.Context,
	userRepo UserRepository,
	apiKeyRepo APIKeyRepository,
	keyName string,
) (internalUsageAttribution, error) {
	if userRepo == nil || apiKeyRepo == nil {
		return internalUsageAttribution{}, fmt.Errorf("usage attribution repositories are not configured")
	}
	admin, err := userRepo.GetFirstAdmin(ctx)
	if err != nil {
		return internalUsageAttribution{}, fmt.Errorf("load first admin: %w", err)
	}
	if admin == nil || admin.ID <= 0 {
		return internalUsageAttribution{}, fmt.Errorf("no active admin user to attribute internal usage to")
	}
	if attr, ok := findInternalUsageKey(ctx, apiKeyRepo, admin.ID, keyName); ok {
		return attr, nil
	}

	secret, err := generateInternalUsageKeySecret()
	if err != nil {
		return internalUsageAttribution{}, err
	}
	key := &APIKey{
		UserID: admin.ID,
		Key:    secret,
		Name:   keyName,
		Status: StatusDisabled,
	}
	if err := apiKeyRepo.Create(ctx, key); err != nil {
		// 多实例并发时可能已被别的实例建好：再查一次，查到就直接用。
		if attr, ok := findInternalUsageKey(ctx, apiKeyRepo, admin.ID, keyName); ok {
			return attr, nil
		}
		return internalUsageAttribution{}, fmt.Errorf("create internal usage key %q: %w", keyName, err)
	}
	if key.ID <= 0 {
		return internalUsageAttribution{}, fmt.Errorf("created internal usage key %q has no id", keyName)
	}
	return internalUsageAttribution{UserID: admin.ID, APIKeyID: key.ID}, nil
}

func findInternalUsageKey(
	ctx context.Context,
	apiKeyRepo APIKeyRepository,
	userID int64,
	keyName string,
) (internalUsageAttribution, bool) {
	keys, err := apiKeyRepo.SearchAPIKeys(ctx, userID, keyName, 20)
	if err != nil {
		return internalUsageAttribution{}, false
	}
	for i := range keys {
		if keys[i].UserID == userID && keys[i].Name == keyName {
			return internalUsageAttribution{UserID: userID, APIKeyID: keys[i].ID}, true
		}
	}
	return internalUsageAttribution{}, false
}

// generateInternalUsageKeySecret 生成专用 Key 的密钥。
// 与 APIKeyService.GenerateKey 同格式（sk- + 十六进制），长度压在列宽以内；
// 该 Key 恒为 disabled，密钥本身不参与任何认证。
func generateInternalUsageKeySecret() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate internal usage key secret: %w", err)
	}
	return "sk-" + hex.EncodeToString(raw), nil
}
