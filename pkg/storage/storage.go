// Copyright (c) 2023 AccelByte Inc. All Rights Reserved.
// This is licensed software from AccelByte Inc, for limitations
// and restrictions contact your company contract manager.

package storage

import (
	"context"
	"encoding/json"

	"github.com/AccelByte/accelbyte-go-sdk/cloudsave-sdk/pkg/cloudsaveclient/admin_game_record"
	"github.com/AccelByte/accelbyte-go-sdk/cloudsave-sdk/pkg/cloudsaveclientmodels"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/cloudsave"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// EnergyData represents the stored energy state in CloudSave
type EnergyData struct {
	UserId           string           `json:"userId"`
	CurrentEnergy    int32            `json:"currentEnergy"`
	MaxEnergy        int32            `json:"maxEnergy"`
	LastUpdateTime   int64            `json:"lastUpdateTime"`   // Unix timestamp
	RegenRateSeconds int32            `json:"regenRateSeconds"` // Seconds per energy point
	Level            int32            `json:"level"`            // Energy system level
	Inventory        map[string]int32 `json:"inventory"`        // item_id -> quantity
}

// Default values for new players
const (
	DefaultMaxEnergy        = 100
	DefaultRegenRateSeconds = 300 // 5 minutes per energy
	DefaultStartingEnergy   = 100 // Start with full energy
	DefaultLevel            = 1
)

// Storage interface for energy data operations
type Storage interface {
	GetEnergyData(ctx context.Context, namespace string, userId string) (*EnergyData, error)
	SaveEnergyData(ctx context.Context, namespace string, userId string, data *EnergyData) (*EnergyData, error)
}

// CloudsaveStorage implements Storage using AccelByte CloudSave
type CloudsaveStorage struct {
	csStorage *cloudsave.AdminGameRecordService
}

// NewCloudSaveStorage creates a new CloudSave storage instance
func NewCloudSaveStorage(csStorage *cloudsave.AdminGameRecordService) *CloudsaveStorage {
	return &CloudsaveStorage{
		csStorage: csStorage,
	}
}

// getEnergyKey returns the CloudSave key for a player's energy data
func getEnergyKey(userId string) string {
	return "energy_" + userId
}

// SaveEnergyData saves energy data to CloudSave
func (c *CloudsaveStorage) SaveEnergyData(ctx context.Context, namespace string, userId string, data *EnergyData) (*EnergyData, error) {
	key := getEnergyKey(userId)

	input := &admin_game_record.AdminPostGameRecordHandlerV1Params{
		Body:      data,
		Key:       key,
		Namespace: namespace,
		Context:   ctx,
	}

	response, err := c.csStorage.AdminPostGameRecordHandlerV1Short(input)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Error saving energy data: %v", err)
	}

	energyData, err := parseResponseToEnergyData(response)
	if err != nil {
		return nil, err
	}

	return energyData, nil
}

// GetEnergyData retrieves energy data from CloudSave
// If no data exists, returns nil (caller should initialize)
func (c *CloudsaveStorage) GetEnergyData(ctx context.Context, namespace string, userId string) (*EnergyData, error) {
	key := getEnergyKey(userId)

	input := &admin_game_record.AdminGetGameRecordHandlerV1Params{
		Key:       key,
		Namespace: namespace,
		Context:   ctx,
	}

	response, err := c.csStorage.AdminGetGameRecordHandlerV1Short(input)
	if err != nil {
		// Check if it's a "not found" error - return nil to indicate new player
		// The service layer will handle initialization
		return nil, nil
	}

	energyData, err := parseResponseToEnergyData(response)
	if err != nil {
		return nil, err
	}

	return energyData, nil
}

// parseResponseToEnergyData converts CloudSave response to EnergyData
func parseResponseToEnergyData(response *cloudsaveclientmodels.ModelsGameRecordAdminResponse) (*EnergyData, error) {
	// Convert the response value to JSON
	valueJSON, err := json.Marshal(response.Value)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Error marshalling value into JSON: %v", err)
	}

	// Unmarshal into EnergyData
	var energyData EnergyData
	err = json.Unmarshal(valueJSON, &energyData)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Error unmarshalling value into EnergyData: %v", err)
	}

	return &energyData, nil
}
