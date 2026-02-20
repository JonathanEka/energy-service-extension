// Copyright (c) 2023 AccelByte Inc. All Rights Reserved.
// This is licensed software from AccelByte Inc, for limitations
// and restrictions contact your company contract manager.

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	pb "extend-custom-guild-service/pkg/pb"
	"extend-custom-guild-service/pkg/storage"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/repository"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// LootEntry defines a possible loot drop
type LootEntry struct {
	ItemID   string
	ItemName string
	MinQty   int32
	MaxQty   int32
	Weight   int // Higher weight = more common
}

// Energy cost per action type (server-authoritative)
var actionEnergyCosts = map[string]int32{
	"fight":   10,
	"explore": 5,
}

// Refill amounts per source type (server-authoritative)
var refillAmounts = map[string]int32{
	"daily":    50, // Daily login bonus
	"ad":       20, // Watch ad reward
	"purchase": 100, // IAP full refill
	"debug":    100, // Debug/testing - full refill
}

// Loot tables per action type
var lootTables = map[string][]LootEntry{
	"fight": {
		{ItemID: "gold", ItemName: "Gold", MinQty: 5, MaxQty: 20, Weight: 50},
		{ItemID: "iron_ore", ItemName: "Iron Ore", MinQty: 1, MaxQty: 3, Weight: 30},
		{ItemID: "gem", ItemName: "Gem", MinQty: 1, MaxQty: 1, Weight: 10},
		{ItemID: "sword_shard", ItemName: "Sword Shard", MinQty: 1, MaxQty: 2, Weight: 10},
	},
	"explore": {
		{ItemID: "gold", ItemName: "Gold", MinQty: 2, MaxQty: 10, Weight: 40},
		{ItemID: "herb", ItemName: "Herb", MinQty: 1, MaxQty: 5, Weight: 35},
		{ItemID: "map_piece", ItemName: "Map Piece", MinQty: 1, MaxQty: 1, Weight: 15},
		{ItemID: "gem", ItemName: "Gem", MinQty: 1, MaxQty: 1, Weight: 10},
	},
}

// rollLoot randomly selects loot based on action type
func rollLoot(actionType string) []*pb.LootItem {
	table, exists := lootTables[actionType]
	if !exists {
		// Default to fight loot if unknown action
		table = lootTables["fight"]
	}

	var loot []*pb.LootItem

	// Roll 1-3 items
	numDrops := rand.Intn(3) + 1

	for i := 0; i < numDrops; i++ {
		// Calculate total weight
		totalWeight := 0
		for _, entry := range table {
			totalWeight += entry.Weight
		}

		// Roll a random number
		roll := rand.Intn(totalWeight)

		// Find which item we landed on
		cumulative := 0
		for _, entry := range table {
			cumulative += entry.Weight
			if roll < cumulative {
				// Roll quantity
				qty := entry.MinQty
				if entry.MaxQty > entry.MinQty {
					qty = entry.MinQty + rand.Int31n(entry.MaxQty-entry.MinQty+1)
				}

				loot = append(loot, &pb.LootItem{
					ItemId:   entry.ItemID,
					ItemName: entry.ItemName,
					Quantity: qty,
				})
				break
			}
		}
	}

	return loot
}

type EnergyServiceServerImpl struct {
	pb.UnimplementedServiceServer
	tokenRepo   repository.TokenRepository
	configRepo  repository.ConfigRepository
	refreshRepo repository.RefreshTokenRepository
	storage     storage.Storage
}

func NewEnergyServiceServer(
	tokenRepo repository.TokenRepository,
	configRepo repository.ConfigRepository,
	refreshRepo repository.RefreshTokenRepository,
	storage storage.Storage,
) *EnergyServiceServerImpl {
	return &EnergyServiceServerImpl{
		tokenRepo:   tokenRepo,
		configRepo:  configRepo,
		refreshRepo: refreshRepo,
		storage:     storage,
	}
}

// ============== PUBLIC ENDPOINTS (Game Client) ==============
// User ID is extracted from the auth token

// GetMyEnergy returns the current energy state for the authenticated player
func (s *EnergyServiceServerImpl) GetMyEnergy(
	ctx context.Context, req *pb.GetMyEnergyRequest,
) (*pb.GetEnergyResponse, error) {
	userId := req.UserId

	energyState, err := s.getOrCreateEnergyState(ctx, req.Namespace, userId)
	if err != nil {
		return nil, err
	}

	return &pb.GetEnergyResponse{EnergyState: energyState}, nil
}

// ConsumeMyEnergy deducts energy for the authenticated player
func (s *EnergyServiceServerImpl) ConsumeMyEnergy(
	ctx context.Context, req *pb.ConsumeMyEnergyRequest,
) (*pb.ConsumeEnergyResponse, error) {
	userId := req.UserId

	// Look up energy cost from server-side config (ignore client-sent amount)
	energyCost, validAction := actionEnergyCosts[req.ActionType]
	if !validAction {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid action type: %s", req.ActionType)
	}

	// Get current state (with regeneration calculated)
	energyState, err := s.getOrCreateEnergyState(ctx, req.Namespace, userId)
	if err != nil {
		return nil, err
	}

	// Check if enough energy
	if energyState.CurrentEnergy < energyCost {
		return &pb.ConsumeEnergyResponse{
			EnergyState: energyState,
			Success:     false,
			Message: fmt.Sprintf("Insufficient energy. Required: %d, Available: %d",
				energyCost, energyState.CurrentEnergy),
		}, nil
	}

	// Get current data to preserve inventory and calculate proper LastUpdateTime
	currentData, _ := s.storage.GetEnergyData(ctx, req.Namespace, userId)
	inventory := make(map[string]int32)
	if currentData != nil && currentData.Inventory != nil {
		inventory = currentData.Inventory
	}

	// Deduct energy (using server-authoritative cost)
	newEnergy := energyState.CurrentEnergy - energyCost
	now := time.Now().Unix()

	// Calculate new LastUpdateTime
	var newLastUpdateTime int64
	if currentData != nil {
		// If energy was at max before this action, start a fresh regen cycle
		// (the old LastUpdateTime is stale since no regen was happening)
		if energyState.CurrentEnergy >= energyState.MaxEnergy {
			newLastUpdateTime = now
		} else {
			// Preserve position in current regen cycle
			elapsed := now - currentData.LastUpdateTime
			regenPoints := elapsed / int64(energyState.RegenRateSeconds)
			newLastUpdateTime = currentData.LastUpdateTime + (regenPoints * int64(energyState.RegenRateSeconds))
		}
	} else {
		newLastUpdateTime = now
	}

	// Roll loot
	loot := rollLoot(req.ActionType)

	// Add loot to inventory
	for _, item := range loot {
		inventory[item.ItemId] += item.Quantity
	}

	// Save updated state with inventory
	updatedData := &storage.EnergyData{
		UserId:           userId,
		CurrentEnergy:    newEnergy,
		MaxEnergy:        energyState.MaxEnergy,
		LastUpdateTime:   newLastUpdateTime,
		RegenRateSeconds: energyState.RegenRateSeconds,
		Level:            1,
		Inventory:        inventory,
	}

	_, err = s.storage.SaveEnergyData(ctx, req.Namespace, userId, updatedData)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to save energy data: %v", err)
	}

	newState := s.calculateEnergyState(updatedData)

	return &pb.ConsumeEnergyResponse{
		EnergyState: newState,
		Success:     true,
		Message:     fmt.Sprintf("Consumed %d energy for %s", energyCost, req.ActionType),
		Loot:        loot,
	}, nil
}

// RefillMyEnergy adds energy for the authenticated player
func (s *EnergyServiceServerImpl) RefillMyEnergy(
	ctx context.Context, req *pb.RefillMyEnergyRequest,
) (*pb.RefillEnergyResponse, error) {
	userId := req.UserId

	// Look up refill amount from server-side config (ignore client-sent amount)
	refillAmount, validSource := refillAmounts[req.Source]
	if !validSource {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid refill source: %s", req.Source)
	}

	// Get current state
	energyState, err := s.getOrCreateEnergyState(ctx, req.Namespace, userId)
	if err != nil {
		return nil, err
	}

	// Get current data to preserve inventory
	currentData, _ := s.storage.GetEnergyData(ctx, req.Namespace, userId)
	inventory := make(map[string]int32)
	if currentData != nil && currentData.Inventory != nil {
		inventory = currentData.Inventory
	}

	// Calculate new energy (capped at max)
	newEnergy := energyState.CurrentEnergy + refillAmount
	if newEnergy > energyState.MaxEnergy {
		newEnergy = energyState.MaxEnergy
	}

	now := time.Now().Unix()

	// Calculate new LastUpdateTime - only advance by actual regen that occurred
	// This preserves the position in the current regen cycle
	var newLastUpdateTime int64
	if currentData != nil {
		elapsed := now - currentData.LastUpdateTime
		regenPoints := elapsed / int64(energyState.RegenRateSeconds)
		// Only advance LastUpdateTime by the time that produced actual regen
		newLastUpdateTime = currentData.LastUpdateTime + (regenPoints * int64(energyState.RegenRateSeconds))
	} else {
		newLastUpdateTime = now
	}

	// Save updated state (preserve inventory)
	updatedData := &storage.EnergyData{
		UserId:           userId,
		CurrentEnergy:    newEnergy,
		MaxEnergy:        energyState.MaxEnergy,
		LastUpdateTime:   newLastUpdateTime,
		RegenRateSeconds: energyState.RegenRateSeconds,
		Level:            1,
		Inventory:        inventory,
	}

	_, err = s.storage.SaveEnergyData(ctx, req.Namespace, userId, updatedData)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to save energy data: %v", err)
	}

	newState := s.calculateEnergyState(updatedData)

	return &pb.RefillEnergyResponse{
		EnergyState: newState,
		Success:     true,
		Message:     fmt.Sprintf("Refilled %d energy from %s", refillAmount, req.Source),
	}, nil
}

// GetMyInventory returns the inventory for the authenticated player
func (s *EnergyServiceServerImpl) GetMyInventory(
	ctx context.Context, req *pb.GetMyInventoryRequest,
) (*pb.GetInventoryResponse, error) {
	userId := req.UserId

	data, err := s.storage.GetEnergyData(ctx, req.Namespace, userId)
	if err != nil {
		return nil, err
	}

	var items []*pb.InventoryItem
	if data != nil && data.Inventory != nil {
		// Convert map to list with item names
		itemNames := map[string]string{
			"gold":        "Gold",
			"iron_ore":    "Iron Ore",
			"gem":         "Gem",
			"sword_shard": "Sword Shard",
			"herb":        "Herb",
			"map_piece":   "Map Piece",
		}

		for itemId, qty := range data.Inventory {
			name := itemId
			if n, ok := itemNames[itemId]; ok {
				name = n
			}
			items = append(items, &pb.InventoryItem{
				ItemId:   itemId,
				ItemName: name,
				Quantity: qty,
			})
		}
	}

	return &pb.GetInventoryResponse{Items: items}, nil
}

// GetMyEnergyConfig returns the energy config for the authenticated player
func (s *EnergyServiceServerImpl) GetMyEnergyConfig(
	ctx context.Context, req *pb.GetMyEnergyConfigRequest,
) (*pb.GetEnergyConfigResponse, error) {
	userId := req.UserId

	data, err := s.storage.GetEnergyData(ctx, req.Namespace, userId)
	if err != nil {
		return nil, err
	}

	// If no data exists, return defaults
	if data == nil {
		return &pb.GetEnergyConfigResponse{
			Config: &pb.EnergyConfig{
				UserId:           userId,
				MaxEnergy:        storage.DefaultMaxEnergy,
				RegenRateSeconds: storage.DefaultRegenRateSeconds,
				Level:            storage.DefaultLevel,
			},
		}, nil
	}

	return &pb.GetEnergyConfigResponse{
		Config: &pb.EnergyConfig{
			UserId:           data.UserId,
			MaxEnergy:        data.MaxEnergy,
			RegenRateSeconds: data.RegenRateSeconds,
			Level:            data.Level,
		},
	}, nil
}

// ============== ADMIN ENDPOINTS (Backend/Tools) ==============
// Explicit user_id in request

// GetEnergy returns the current energy state for a player (admin)
func (s *EnergyServiceServerImpl) GetEnergy(
	ctx context.Context, req *pb.GetEnergyRequest,
) (*pb.GetEnergyResponse, error) {
	energyState, err := s.getOrCreateEnergyState(ctx, req.Namespace, req.UserId)
	if err != nil {
		return nil, err
	}

	return &pb.GetEnergyResponse{EnergyState: energyState}, nil
}

// ConsumeEnergy deducts energy for an action (admin)
func (s *EnergyServiceServerImpl) ConsumeEnergy(
	ctx context.Context, req *pb.ConsumeEnergyRequest,
) (*pb.ConsumeEnergyResponse, error) {
	// Validate amount
	if req.Amount <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "Amount must be positive")
	}

	// Get current state (with regeneration calculated)
	energyState, err := s.getOrCreateEnergyState(ctx, req.Namespace, req.UserId)
	if err != nil {
		return nil, err
	}

	// Check if enough energy
	if energyState.CurrentEnergy < req.Amount {
		return &pb.ConsumeEnergyResponse{
			EnergyState: energyState,
			Success:     false,
			Message: fmt.Sprintf("Insufficient energy. Required: %d, Available: %d",
				req.Amount, energyState.CurrentEnergy),
		}, nil
	}

	// Deduct energy
	newEnergy := energyState.CurrentEnergy - req.Amount
	now := time.Now().Unix()

	// Save updated state
	updatedData := &storage.EnergyData{
		UserId:           req.UserId,
		CurrentEnergy:    newEnergy,
		MaxEnergy:        energyState.MaxEnergy,
		LastUpdateTime:   now,
		RegenRateSeconds: energyState.RegenRateSeconds,
		Level:            1,
	}

	_, err = s.storage.SaveEnergyData(ctx, req.Namespace, req.UserId, updatedData)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to save energy data: %v", err)
	}

	newState := s.calculateEnergyState(updatedData)

	return &pb.ConsumeEnergyResponse{
		EnergyState: newState,
		Success:     true,
		Message:     fmt.Sprintf("Consumed %d energy for %s", req.Amount, req.ActionType),
	}, nil
}

// RefillEnergy adds energy to the player's pool (admin)
func (s *EnergyServiceServerImpl) RefillEnergy(
	ctx context.Context, req *pb.RefillEnergyRequest,
) (*pb.RefillEnergyResponse, error) {
	// Validate amount
	if req.Amount <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "Amount must be positive")
	}

	// Get current state
	energyState, err := s.getOrCreateEnergyState(ctx, req.Namespace, req.UserId)
	if err != nil {
		return nil, err
	}

	// Calculate new energy (capped at max)
	newEnergy := energyState.CurrentEnergy + req.Amount
	if newEnergy > energyState.MaxEnergy {
		newEnergy = energyState.MaxEnergy
	}

	now := time.Now().Unix()

	// Save updated state
	updatedData := &storage.EnergyData{
		UserId:           req.UserId,
		CurrentEnergy:    newEnergy,
		MaxEnergy:        energyState.MaxEnergy,
		LastUpdateTime:   now,
		RegenRateSeconds: energyState.RegenRateSeconds,
		Level:            1,
	}

	_, err = s.storage.SaveEnergyData(ctx, req.Namespace, req.UserId, updatedData)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to save energy data: %v", err)
	}

	newState := s.calculateEnergyState(updatedData)

	return &pb.RefillEnergyResponse{
		EnergyState: newState,
		Success:     true,
		Message:     fmt.Sprintf("Refilled %d energy from %s", req.Amount, req.Source),
	}, nil
}

// GetEnergyConfig returns the player's energy configuration (admin)
func (s *EnergyServiceServerImpl) GetEnergyConfig(
	ctx context.Context, req *pb.GetEnergyConfigRequest,
) (*pb.GetEnergyConfigResponse, error) {
	data, err := s.storage.GetEnergyData(ctx, req.Namespace, req.UserId)
	if err != nil {
		return nil, err
	}

	// If no data exists, return defaults
	if data == nil {
		return &pb.GetEnergyConfigResponse{
			Config: &pb.EnergyConfig{
				UserId:           req.UserId,
				MaxEnergy:        storage.DefaultMaxEnergy,
				RegenRateSeconds: storage.DefaultRegenRateSeconds,
				Level:            storage.DefaultLevel,
			},
		}, nil
	}

	return &pb.GetEnergyConfigResponse{
		Config: &pb.EnergyConfig{
			UserId:           data.UserId,
			MaxEnergy:        data.MaxEnergy,
			RegenRateSeconds: data.RegenRateSeconds,
			Level:            data.Level,
		},
	}, nil
}

// UpdateEnergyConfig updates the player's energy configuration (admin)
func (s *EnergyServiceServerImpl) UpdateEnergyConfig(
	ctx context.Context, req *pb.UpdateEnergyConfigRequest,
) (*pb.UpdateEnergyConfigResponse, error) {
	// Get current state
	energyState, err := s.getOrCreateEnergyState(ctx, req.Namespace, req.UserId)
	if err != nil {
		return nil, err
	}

	// Apply updates (0 = no change)
	newMaxEnergy := energyState.MaxEnergy
	newRegenRate := energyState.RegenRateSeconds

	if req.MaxEnergy > 0 {
		newMaxEnergy = req.MaxEnergy
	}
	if req.RegenRateSeconds > 0 {
		newRegenRate = req.RegenRateSeconds
	}

	now := time.Now().Unix()

	// Save updated config
	updatedData := &storage.EnergyData{
		UserId:           req.UserId,
		CurrentEnergy:    energyState.CurrentEnergy,
		MaxEnergy:        newMaxEnergy,
		LastUpdateTime:   now,
		RegenRateSeconds: newRegenRate,
		Level:            1,
	}

	_, err = s.storage.SaveEnergyData(ctx, req.Namespace, req.UserId, updatedData)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to save config: %v", err)
	}

	return &pb.UpdateEnergyConfigResponse{
		Config: &pb.EnergyConfig{
			UserId:           req.UserId,
			MaxEnergy:        newMaxEnergy,
			RegenRateSeconds: newRegenRate,
			Level:            1,
		},
		Success: true,
		Message: "Energy configuration updated",
	}, nil
}

// ResetEnergy resets a player's energy state to defaults (admin only)
func (s *EnergyServiceServerImpl) ResetEnergy(
	ctx context.Context, req *pb.ResetEnergyRequest,
) (*pb.ResetEnergyResponse, error) {
	now := time.Now().Unix()

	// Create fresh default state
	defaultData := &storage.EnergyData{
		UserId:           req.UserId,
		CurrentEnergy:    storage.DefaultStartingEnergy,
		MaxEnergy:        storage.DefaultMaxEnergy,
		LastUpdateTime:   now,
		RegenRateSeconds: storage.DefaultRegenRateSeconds,
		Level:            storage.DefaultLevel,
	}

	_, err := s.storage.SaveEnergyData(ctx, req.Namespace, req.UserId, defaultData)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to reset energy: %v", err)
	}

	newState := s.calculateEnergyState(defaultData)

	return &pb.ResetEnergyResponse{
		EnergyState: newState,
		Success:     true,
		Message:     "Energy state reset to defaults",
	}, nil
}

// ============== Helper Methods ==============

// extractUserIdFromToken extracts the user ID from the JWT token in the context
func (s *EnergyServiceServerImpl) extractUserIdFromToken(ctx context.Context) (string, error) {
	meta, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Errorf(codes.Unauthenticated, "missing metadata")
	}

	authHeader := meta.Get("authorization")
	if len(authHeader) == 0 {
		return "", status.Errorf(codes.Unauthenticated, "missing authorization header")
	}

	token := strings.TrimPrefix(authHeader[0], "Bearer ")

	// JWT has 3 parts: header.payload.signature
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", status.Errorf(codes.Unauthenticated, "invalid token format")
	}

	// Decode payload (middle part)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", status.Errorf(codes.Unauthenticated, "failed to decode token payload")
	}

	// Parse JSON to extract sub (user ID)
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", status.Errorf(codes.Unauthenticated, "failed to parse token claims")
	}

	if claims.Sub == "" {
		return "", status.Errorf(codes.Unauthenticated, "user ID not found in token")
	}

	return claims.Sub, nil
}

// getOrCreateEnergyState gets existing energy or creates default for new players
func (s *EnergyServiceServerImpl) getOrCreateEnergyState(
	ctx context.Context, namespace string, userId string,
) (*pb.EnergyState, error) {
	data, err := s.storage.GetEnergyData(ctx, namespace, userId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get energy data: %v", err)
	}

	// New player - create default energy state
	if data == nil {
		now := time.Now().Unix()
		data = &storage.EnergyData{
			UserId:           userId,
			CurrentEnergy:    storage.DefaultStartingEnergy,
			MaxEnergy:        storage.DefaultMaxEnergy,
			LastUpdateTime:   now,
			RegenRateSeconds: storage.DefaultRegenRateSeconds,
			Level:            storage.DefaultLevel,
		}

		// Save the initial state
		_, err = s.storage.SaveEnergyData(ctx, namespace, userId, data)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to initialize energy: %v", err)
		}
	}

	// Calculate current energy with regeneration
	return s.calculateEnergyState(data), nil
}

// calculateEnergyState applies time-based regeneration to stored data
func (s *EnergyServiceServerImpl) calculateEnergyState(data *storage.EnergyData) *pb.EnergyState {
	now := time.Now().Unix()

	// Calculate regenerated energy since last update
	elapsedSeconds := now - data.LastUpdateTime
	regenPoints := int32(0)

	if data.RegenRateSeconds > 0 && elapsedSeconds > 0 {
		regenPoints = int32(elapsedSeconds / int64(data.RegenRateSeconds))
	}

	// Apply regeneration (capped at max)
	currentEnergy := data.CurrentEnergy + regenPoints
	if currentEnergy > data.MaxEnergy {
		currentEnergy = data.MaxEnergy
	}

	// Calculate time to next regen and time to max
	energyToMax := data.MaxEnergy - currentEnergy
	var nextRegenTime int64 = 0
	var timeToMaxSeconds int64 = 0

	if currentEnergy < data.MaxEnergy {
		// Time until next energy point
		usedSeconds := elapsedSeconds % int64(data.RegenRateSeconds)
		secondsToNext := int64(data.RegenRateSeconds) - usedSeconds
		nextRegenTime = now + secondsToNext

		// Total time to reach max
		timeToMaxSeconds = int64(energyToMax) * int64(data.RegenRateSeconds)
		// Subtract the partial regen time
		timeToMaxSeconds -= usedSeconds
	}

	return &pb.EnergyState{
		UserId:           data.UserId,
		CurrentEnergy:    currentEnergy,
		MaxEnergy:        data.MaxEnergy,
		LastUpdateTime:   data.LastUpdateTime,
		RegenRateSeconds: data.RegenRateSeconds,
		NextRegenTime:    nextRegenTime,
		EnergyToMax:      energyToMax,
		TimeToMaxSeconds: timeToMaxSeconds,
	}
}
