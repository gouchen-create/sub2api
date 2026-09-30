package service

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ProvideReconciliationA6Config 从应用配置构造 A6 客户端配置。
//
// 这里返回的是「配置层默认值」：面板上的覆盖值由 ReconciliationA6SettingsService
// 在这一层之上解析，因此本函数不需要知道面板的存在。
func ProvideReconciliationA6Config(cfg *config.Config) ReconciliationA6Config {
	return ReconciliationA6Config{
		BaseURL:     cfg.Reconciliation.A6BaseURL,
		AccessToken: cfg.Reconciliation.A6AccessToken,
		UserID:      cfg.Reconciliation.A6UserID,
		Timeout:     time.Duration(cfg.Reconciliation.TimeoutSeconds) * time.Second,
	}
}

// ProvideReconciliationSyncConfig 把配置里的「小时/秒」翻译成 time.Duration。
//
// 抽成独立 provider 是因为同步服务与 A6 设置服务（汇率默认值）需要同一份参数：
// 两边各算一份，迟早会漂移成两个不一样的汇率。
func ProvideReconciliationSyncConfig(cfg *config.Config) ReconciliationSyncConfig {
	recon := cfg.Reconciliation
	return ReconciliationSyncConfig{
		FxUSDCNYRate: recon.FxUSDCNYRate,
		A6Lookback:   time.Duration(recon.A6LookbackHours) * time.Hour,
	}
}

// ProvideReconciliationA6Settings 构造 A6 生效凭据服务。
//
// defaults 是配置层（环境变量 / 配置文件）注入的默认值，面板覆盖优先于它。
func ProvideReconciliationA6Settings(
	stateRepo ReconciliationSyncStateRepository,
	encryptor SecretEncryptor,
	defaults ReconciliationA6Config,
	syncCfg ReconciliationSyncConfig,
) *ReconciliationA6SettingsService {
	return NewReconciliationA6SettingsService(stateRepo, encryptor, defaults, syncCfg.FxUSDCNYRate)
}

// ProvideReconciliationSyncService 构造对账同步服务。
//
// 时长与参数由 ProvideReconciliationSyncConfig 统一翻译好，这里只做透传；
// 非法或零值由构造函数兜底成默认值，因此这里不做校验。
func ProvideReconciliationSyncService(
	extrasRepo ReconciliationUsageExtraRepository,
	billRepo ReconciliationUpstreamBillRepository,
	ruleRepo ReconciliationAccountRuleRepository,
	stateRepo ReconciliationSyncStateRepository,
	usageSource ReconciliationUsageSource,
	billSource ReconciliationUpstreamBillSource,
	syncCfg ReconciliationSyncConfig,
) *ReconciliationSyncService {
	return NewReconciliationSyncService(
		extrasRepo,
		billRepo,
		ruleRepo,
		stateRepo,
		usageSource,
		billSource,
		syncCfg,
	)
}
