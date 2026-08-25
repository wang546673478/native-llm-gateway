package database

import (
	"context"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openTestPG 连真库,没配 DSN 就 skip —— 不让单测依赖外部服务。
func openTestPG(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LLMGW_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LLMGW_TEST_PG_DSN 未设置,跳过(需要真 PG 才能验 GORM 零值跳过行为)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	return db
}

// TestCreate_EnabledFalse_NotSwallowed 守卫:Enabled=false 必须真的落库成 false。
//
// GORM 的 Create 会跳过零值字段,让 DB 的 DEFAULT 去填 —— 而这几张表的 enabled 列
// 恰好是 `default:true`。于是 `Enabled: false` 被跳过、DB 填 true,用户在 UI 上取消
// 勾选"启用"反而建出一个启用的行。中转站尤其危险:热重载会立刻把它加载进路由池,
// 一个还没配好的站就开始接流量(2026-08-25 实测 create 传 false 返回 true)。
//
// 三张表共用同一个 `bool + default:true` 形状,同源同病,一起盯。
// 全程事务内回滚,对生产零副作用。
func TestCreate_EnabledFalse_NotSwallowed(t *testing.T) {
	db := openTestPG(t)
	now := time.Now().UTC()

	cases := []struct {
		table string
		// create 用被测的 Store 实现,而不是裸 db.Create —— 要验的是生产路径
		create func(tx *gorm.DB) (id uint, err error)
		read   func(tx *gorm.DB, id uint) (enabled bool, err error)
	}{
		{
			table: "relay_stations",
			create: func(tx *gorm.DB) (uint, error) {
				s := RelayStation{
					Name: "__zv_probe__", BaseURL: "https://example.invalid",
					ProtocolMode: "single", PrimaryProtocol: "openai",
					BillingSource: "api", Timeout: 60,
					Enabled: BoolPtr(false), // 关键
				}
				err := NewRelayStationStore(tx).Create(context.Background(), &s)
				return s.ID, err
			},
			read: func(tx *gorm.DB, id uint) (bool, error) {
				var v bool
				err := tx.Raw("SELECT enabled FROM relay_stations WHERE id = ?", id).Scan(&v).Error
				return v, err
			},
		},
		{
			table: "gateway_keys",
			create: func(tx *gorm.DB) (uint, error) {
				k := GatewayKey{
					Name: "__zv_probe__", KeyHash: "__zv_probe_hash__",
					AllowedModels: `["*"]`, RPM: 100, TPM: 500000,
					CreatedAt: now, UpdatedAt: now,
					Enabled: BoolPtr(false), // 关键
				}
				err := tx.Create(&k).Error
				return k.ID, err
			},
			read: func(tx *gorm.DB, id uint) (bool, error) {
				var v bool
				err := tx.Raw("SELECT enabled FROM gateway_keys WHERE id = ?", id).Scan(&v).Error
				return v, err
			},
		},
		{
			table: "provider_api_keys",
			create: func(tx *gorm.DB) (uint, error) {
				k := ProviderAPIKey{
					ProviderName: "__zv_probe__", Name: "__zv_probe__",
					KeyHash: "__zv_probe_hash__", BillingSource: "api",
					CreatedAt: now,
					Enabled:   BoolPtr(false), // 关键
				}
				err := tx.Create(&k).Error
				return k.ID, err
			},
			read: func(tx *gorm.DB, id uint) (bool, error) {
				var v bool
				err := tx.Raw("SELECT enabled FROM provider_api_keys WHERE id = ?", id).Scan(&v).Error
				return v, err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			// 事务内建 + 读 + 强制回滚
			err := db.Transaction(func(tx *gorm.DB) error {
				id, err := tc.create(tx)
				if err != nil {
					return err
				}
				if id == 0 {
					t.Fatal("INSERT 成功但没回填 ID")
				}
				got, err := tc.read(tx, id)
				if err != nil {
					return err
				}
				if got {
					t.Errorf("%s: 传 Enabled=false,落库成 true —— GORM 跳过了 bool 零值,"+
						"DB 的 default:true 顶了上来。用户取消勾选\"启用\"却建出启用的行", tc.table)
				}
				return gorm.ErrInvalidTransaction // 强制回滚
			})
			if err != nil && err != gorm.ErrInvalidTransaction {
				t.Fatalf("%s: create/read 失败: %v", tc.table, err)
			}
		})
	}
}

// zvCanary 只在本测试里用:复刻修复前的字段形状(裸 bool + default:true)。
type zvCanary struct {
	ID      uint `gorm:"primarykey"`
	Enabled bool `gorm:"column:enabled;not null;default:true"`
}

func (zvCanary) TableName() string { return "zz_zv_canary" }

// TestGORM_SkipsZeroValueBool_Canary 钉住上面那个守卫赖以成立的前提。
//
// 上面三个 case 现在是绿的,靠的是把字段改成了 *bool。但"绿"本身不证明守卫还在
// 干活 —— 万一哪天 GORM 改了行为、裸 bool 也能正常写 false,那三个 case 就算把
// *bool 改回 bool 也依然绿,守卫静默失效而没人知道。
//
// 所以这里反着断言:用裸 bool + default:true 的形状,必须复现出"传 false 落库
// 成 true"这个 bug。它红了,说明 GORM 不再跳零值 —— 那时候上面的 *bool 就是
// 多余的,可以连这个 canary 一起删。
// 临时表建在事务里,回滚即消失,不碰生产 schema。
func TestGORM_SkipsZeroValueBool_Canary(t *testing.T) {
	db := openTestPG(t)

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Migrator().CreateTable(&zvCanary{}); err != nil {
			return err
		}
		row := zvCanary{Enabled: false}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		var got bool
		if err := tx.Raw("SELECT enabled FROM zz_zv_canary WHERE id = ?", row.ID).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			t.Error("裸 bool 传 false 竟然落库成了 false —— GORM 不再跳过带 default tag 的零值。" +
				"前提变了:models.go 里 Enabled 的 *bool 已无必要,可连本 canary 一起删")
		}
		return gorm.ErrInvalidTransaction // 强制回滚,临时表随之消失
	})
	if err != nil && err != gorm.ErrInvalidTransaction {
		t.Fatalf("canary 建表/写入失败: %v", err)
	}
}
