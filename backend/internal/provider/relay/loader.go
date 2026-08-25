package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"gorm.io/gorm"
)

// ProviderManager 是 Manager 的窄接口，避免 relay 包依赖完整的 Manager
type ProviderManager interface {
	AddProvider(ctx context.Context, name string, p provider.Provider) error
	RemoveProvider(name string)
	GetAll() map[string]provider.Provider
}

// LoadFromDatabase 从数据库加载所有启用的中转站并注册到 provider registry 和 manager
func LoadFromDatabase(db *gorm.DB, mgr ProviderManager) error {
	var stations []database.RelayStation
	if err := db.Where("enabled = ?", true).Find(&stations).Error; err != nil {
		return fmt.Errorf("query relay stations: %w", err)
	}

	log.Printf("[relay] Loading %d enabled relay station(s) from database", len(stations))

	ctx := context.Background()
	for _, s := range stations {
		// 同步 keys 到 provider_api_keys 表
		if err := syncRelayStationKeys(db, s); err != nil {
			log.Printf("[relay] Failed to sync keys for %s: %v", s.Name, err)
			// 不中断流程,继续注册
		}

		if err := registerAndLoadRelayStation(ctx, s, mgr); err != nil {
			log.Printf("[relay] Failed to register %s: %v", s.Name, err)
			continue
		}
		log.Printf("[relay] Registered relay station: %s (mode=%s, protocol=%s, url=%s)",
			s.Name, s.ProtocolMode, s.PrimaryProtocol, s.BaseURL)
	}

	return nil
}

// registerAndLoadRelayStation 注册单个中转站到 provider registry 并加载到 manager
func registerAndLoadRelayStation(ctx context.Context, s database.RelayStation, mgr ProviderManager) error {
	// 解析支持的协议列表
	var supportedProtocols []provider.Protocol
	if s.SupportedProtocols != "" {
		if err := json.Unmarshal([]byte(s.SupportedProtocols), &supportedProtocols); err != nil {
			return fmt.Errorf("parse supported_protocols: %w", err)
		}
	}

	// 单协议模式:只支持主协议
	if s.ProtocolMode == "single" {
		supportedProtocols = []provider.Protocol{provider.Protocol(s.PrimaryProtocol)}
	}

	// 多协议模式:如果没有指定支持的协议,默认支持主协议
	if s.ProtocolMode == "multi" && len(supportedProtocols) == 0 {
		supportedProtocols = []provider.Protocol{provider.Protocol(s.PrimaryProtocol)}
	}

	// 创建 GenericRelayProvider
	relayProvider, err := NewGenericRelayProvider(Config{
		Name:               s.Name,
		BaseURL:            s.BaseURL,
		ProtocolMode:       s.ProtocolMode,
		PrimaryProtocol:    provider.Protocol(s.PrimaryProtocol),
		SupportedProtocols: supportedProtocols,
		Timeout:            s.Timeout,
		ProtocolConfigs:    make(map[provider.Protocol]interface{}),
	})
	if err != nil {
		return err
	}

	// 注册到 provider registry
	// 多协议模式:为每个支持的协议注册一个独立的面
	registry := provider.Default()
	if s.ProtocolMode == "multi" {
		for _, proto := range supportedProtocols {
			faceName := fmt.Sprintf("%s-%s", s.Name, proto)
			registry.RegisterWithProtocolVendorRelay(faceName, func(cfg provider.ProviderConfig) (provider.Provider, error) {
				return relayProvider, nil
			}, proto, s.Name, true) // vendor = name

			// 加载到 manager
			if err := mgr.AddProvider(ctx, faceName, relayProvider); err != nil {
				return fmt.Errorf("add provider %s to manager: %w", faceName, err)
			}
		}
	} else {
		// 单协议模式:直接注册
		registry.RegisterWithProtocolVendorRelay(s.Name, func(cfg provider.ProviderConfig) (provider.Provider, error) {
			return relayProvider, nil
		}, provider.Protocol(s.PrimaryProtocol), s.Name, true) // vendor = name

		// 加载到 manager
		if err := mgr.AddProvider(ctx, s.Name, relayProvider); err != nil {
			return fmt.Errorf("add provider %s to manager: %w", s.Name, err)
		}
	}

	return nil
}

// syncRelayStationKeys 将中转站的 keys 同步到 provider_api_keys 表
// 策略：以 relay_stations.keys 为准，删除 provider_api_keys 中多余的，添加缺失的
func syncRelayStationKeys(db *gorm.DB, s database.RelayStation) error {
	// 解析 JSON keys
	var keys []string
	if s.Keys != "" {
		if err := json.Unmarshal([]byte(s.Keys), &keys); err != nil {
			return fmt.Errorf("parse keys JSON: %w", err)
		}
	}

	// 查询当前 provider_api_keys 中该 provider 的所有 keys
	var existingKeys []database.ProviderAPIKey
	if err := db.Where("provider_name = ?", s.Name).Find(&existingKeys).Error; err != nil {
		return fmt.Errorf("query existing keys: %w", err)
	}

	// 构建目标 key 名集合 (使用 key 的最后 8 位作为名称)
	targetKeyNames := make(map[string]string) // name -> key_hash
	for _, key := range keys {
		if len(key) >= 8 {
			name := key[len(key)-8:]
			targetKeyNames[name] = key
		}
	}

	// 删除不在目标集合中的 keys
	for _, ek := range existingKeys {
		if _, exists := targetKeyNames[ek.Name]; !exists {
			if err := db.Delete(&ek).Error; err != nil {
				log.Printf("[relay] Failed to delete key %s for %s: %v", ek.Name, s.Name, err)
			} else {
				log.Printf("[relay] Deleted key %s for %s", ek.Name, s.Name)
			}
		}
	}

	// 添加缺失的 keys
	existingKeyNames := make(map[string]bool)
	for _, ek := range existingKeys {
		existingKeyNames[ek.Name] = true
	}

	for name, keyHash := range targetKeyNames {
		if !existingKeyNames[name] {
			newKey := database.ProviderAPIKey{
				ProviderName:   s.Name,
				Name:           name,
				KeyHash:        keyHash,
				Enabled:        database.BoolPtr(true),
				BillingSource:  "api", // 中转站默认 api
				Protocols:      "",    // 空表示支持所有协议
			}
			if err := db.Create(&newKey).Error; err != nil {
				log.Printf("[relay] Failed to create key %s for %s: %v", name, s.Name, err)
			} else {
				log.Printf("[relay] Created key %s for %s", name, s.Name)
			}
		}
	}

	return nil
}

// ReloadFromDatabase 热重载所有中转站 — 先删除所有已注册的中转站，再重新加载
func ReloadFromDatabase(db *gorm.DB, mgr ProviderManager) error {
	// 1. 找出所有已加载的中转站
	registry := provider.Default()
	toRemove := make([]string, 0)
	for name := range mgr.GetAll() {
		if registry.IsRelay(name) {
			toRemove = append(toRemove, name)
		}
	}

	// 2. 删除所有中转站
	for _, name := range toRemove {
		mgr.RemoveProvider(name)
		log.Printf("[relay] Removed relay station: %s", name)
	}

	// 3. 重新加载
	return LoadFromDatabase(db, mgr)
}
