// Package database — RelayStation Store 实现
package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// RelayStationStore 中转站配置存储接口
type RelayStationStore interface {
	List(ctx context.Context) ([]RelayStation, error)
	Get(ctx context.Context, id uint) (*RelayStation, error)
	Create(ctx context.Context, station *RelayStation) error
	Update(ctx context.Context, station *RelayStation) error
	Delete(ctx context.Context, id uint) error
}

// relayStationStore 实现
type relayStationStore struct {
	db *gorm.DB
}

// NewRelayStationStore 创建 RelayStationStore
func NewRelayStationStore(db *gorm.DB) RelayStationStore {
	return &relayStationStore{db: db}
}

// List 查询所有中转站
func (s *relayStationStore) List(ctx context.Context) ([]RelayStation, error) {
	var stations []RelayStation
	if err := s.db.WithContext(ctx).Order("created_at DESC").Find(&stations).Error; err != nil {
		return nil, fmt.Errorf("list relay stations: %w", err)
	}
	return stations, nil
}

// Get 根据 ID 查询单个中转站
func (s *relayStationStore) Get(ctx context.Context, id uint) (*RelayStation, error) {
	var station RelayStation
	if err := s.db.WithContext(ctx).First(&station, id).Error; err != nil {
		return nil, fmt.Errorf("get relay station %d: %w", id, err)
	}
	return &station, nil
}

// Create 创建中转站
func (s *relayStationStore) Create(ctx context.Context, station *RelayStation) error {
	if err := s.db.WithContext(ctx).Create(station).Error; err != nil {
		return fmt.Errorf("create relay station: %w", err)
	}
	return nil
}

// Update 更新中转站
func (s *relayStationStore) Update(ctx context.Context, station *RelayStation) error {
	if err := s.db.WithContext(ctx).Save(station).Error; err != nil {
		return fmt.Errorf("update relay station: %w", err)
	}
	return nil
}

// Delete 删除中转站
func (s *relayStationStore) Delete(ctx context.Context, id uint) error {
	if err := s.db.WithContext(ctx).Delete(&RelayStation{}, id).Error; err != nil {
		return fmt.Errorf("delete relay station: %w", err)
	}
	return nil
}
