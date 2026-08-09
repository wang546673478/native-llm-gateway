package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// RouteOrderStore 访问 route_order 排序改写表(Level 2/3 优先级改写)。
// 只存「用户改写」;默认排序(未改写)由 created_at 派生,本表无行 = 零代价。
type RouteOrderStore interface {
	// ListByScope 取某作用域的全部改写(Scope=provider → 层内 provider 排序;
	// Scope=key 且 Provider 非空 → 某 provider 内 key 排序)。
	ListByScope(ctx context.Context, scope string, provider string) ([]RouteOrder, error)
	// Replace 整体替换某作用域的改写列表(前端拖拽后落库)。
	// 同一 (scope,provider,billing_source) 作用域先删后插,保证与界面一致。
	Replace(ctx context.Context, scope, provider, billingSource string, names []string) error
	// ResetScope 删除某作用域全部改写 → 回到默认 created_at 顺序。
	ResetScope(ctx context.Context, scope string, provider string) error
}

type gormRouteOrderStore struct{ db *gorm.DB }

// NewRouteOrderStore 构造实现
func NewRouteOrderStore(db *gorm.DB) RouteOrderStore { return &gormRouteOrderStore{db: db} }

func (s *gormRouteOrderStore) ListByScope(ctx context.Context, scope string, provider string) ([]RouteOrder, error) {
	var out []RouteOrder
	q := s.db.WithContext(ctx).Model(&RouteOrder{}).Where("scope = ?", scope)
	if provider != "" {
		q = q.Where("provider = ?", provider)
	}
	if err := q.Order("seq ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *gormRouteOrderStore) Replace(ctx context.Context, scope, provider, billingSource string, names []string) error {
	// 一次事务:先删该作用域,再插入新顺序。
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Where("scope = ?", scope)
		if provider != "" {
			q = q.Where("provider = ?", provider)
		}
		if err := q.Delete(&RouteOrder{}).Error; err != nil {
			return fmt.Errorf("clear route_order: %w", err)
		}
		for i, n := range names {
			r := RouteOrder{
				Scope:         scope,
				Provider:      provider,
				Name:          n,
				BillingSource: billingSource,
				Seq:           i, // 小在前
			}
			if err := tx.Create(&r).Error; err != nil {
				return fmt.Errorf("insert route_order %q: %w", n, err)
			}
		}
		return nil
	})
}

func (s *gormRouteOrderStore) ResetScope(ctx context.Context, scope string, provider string) error {
	q := s.db.WithContext(ctx).Where("scope = ?", scope)
	if provider != "" {
		q = q.Where("provider = ?", provider)
	}
	return q.Delete(&RouteOrder{}).Error
}
