package repository

import (
	"errors"
	"time"

	"github.com/RAF-SI-2025/EXBanka-3-Backend/exchange-service/internal/models"
	"gorm.io/gorm"
)

// InterbankOtcRepository persists our local copy of every cross-bank
// OTC negotiation. Each row is keyed by (NegotiationRoutingNumber,
// NegotiationID) — the seller bank's coordinates.
type InterbankOtcRepository struct {
	db *gorm.DB
}

func NewInterbankOtcRepository(db *gorm.DB) *InterbankOtcRepository {
	return &InterbankOtcRepository{db: db}
}

// Create persists a new negotiation. Timestamps are set here so
// callers don't have to remember.
func (r *InterbankOtcRepository) Create(neg *models.InterbankOtcNegotiation) error {
	now := time.Now().UTC()
	neg.CreatedAt = now
	neg.UpdatedAt = now
	if !neg.IsOngoing {
		neg.IsOngoing = true
	}
	return r.db.Create(neg).Error
}

// Get fetches a negotiation by its global key. Returns (nil, nil)
// when no row exists — callers branch on that for "negotiation does
// not exist on our side yet" cases (which happen on the first
// inbound POST /negotiations).
func (r *InterbankOtcRepository) Get(negotiationRoutingNumber int, negotiationID string) (*models.InterbankOtcNegotiation, error) {
	var neg models.InterbankOtcNegotiation
	err := r.db.
		Where("negotiation_routing_number = ? AND negotiation_id = ?", negotiationRoutingNumber, negotiationID).
		First(&neg).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &neg, nil
}

// UpdateTerms applies a counter-offer's term changes. Caller supplies
// the new amount, price, premium, settlement date, and the identity
// of whoever made the change. updated_at is set automatically.
func (r *InterbankOtcRepository) UpdateTerms(
	negotiationRoutingNumber int,
	negotiationID string,
	amount float64,
	pricePerUnitCurrency string, pricePerUnitAmount float64,
	premiumCurrency string, premiumAmount float64,
	settlementDate string,
	modifiedByRoutingNumber int, modifiedByID string,
) error {
	return r.db.Model(&models.InterbankOtcNegotiation{}).
		Where("negotiation_routing_number = ? AND negotiation_id = ?", negotiationRoutingNumber, negotiationID).
		Updates(map[string]interface{}{
			"amount":                          amount,
			"price_per_unit_currency":         pricePerUnitCurrency,
			"price_per_unit_amount":           pricePerUnitAmount,
			"premium_currency":                premiumCurrency,
			"premium_amount":                  premiumAmount,
			"settlement_date":                 settlementDate,
			"last_modified_by_routing_number": modifiedByRoutingNumber,
			"last_modified_by_id":             modifiedByID,
			"updated_at":                      time.Now().UTC(),
		}).Error
}

// MarkClosed flips IsOngoing=false. Used when either side accepts or
// declines the negotiation. Acceptance is not modelled separately
// because the protocol expresses it as a side-effect of GET
// /negotiations/{...}/accept (which triggers NEW_TX); the row's only
// state change is "ongoing → closed".
func (r *InterbankOtcRepository) MarkClosed(negotiationRoutingNumber int, negotiationID string) error {
	return r.db.Model(&models.InterbankOtcNegotiation{}).
		Where("negotiation_routing_number = ? AND negotiation_id = ?", negotiationRoutingNumber, negotiationID).
		Updates(map[string]interface{}{
			"is_ongoing": false,
			"updated_at": time.Now().UTC(),
		}).Error
}

// MarkOngoing flips IsOngoing back to true. The accept handler calls
// this when NEW_TX dispatch fails or the buyer's bank votes NO — the
// negotiation reopens so the participants can keep haggling. The
// LastModifiedBy fields are reset to whoever last touched the row so
// the wire copy stays consistent with what the partner already saw.
func (r *InterbankOtcRepository) MarkOngoing(negotiationRoutingNumber int, negotiationID string, lastModRouting int, lastModID string) error {
	return r.db.Model(&models.InterbankOtcNegotiation{}).
		Where("negotiation_routing_number = ? AND negotiation_id = ?", negotiationRoutingNumber, negotiationID).
		Updates(map[string]interface{}{
			"is_ongoing":                      true,
			"last_modified_by_routing_number": lastModRouting,
			"last_modified_by_id":             lastModID,
			"updated_at":                      time.Now().UTC(),
		}).Error
}

// ListByLocalParticipant returns the open negotiations where a given
// local user is one of the two sides. role filters to "buyer" or
// "seller"; pass "" for both.
func (r *InterbankOtcRepository) ListByLocalParticipant(localID string, role string, includeClosed bool) ([]models.InterbankOtcNegotiation, error) {
	query := r.db.Model(&models.InterbankOtcNegotiation{})
	switch role {
	case models.InterbankNegotiationRoleBuyer:
		query = query.Where("buyer_id = ? AND local_role = ?", localID, models.InterbankNegotiationRoleBuyer)
	case models.InterbankNegotiationRoleSeller:
		query = query.Where("seller_id = ? AND local_role = ?", localID, models.InterbankNegotiationRoleSeller)
	case "":
		query = query.Where(
			"(local_role = ? AND buyer_id = ?) OR (local_role = ? AND seller_id = ?)",
			models.InterbankNegotiationRoleBuyer, localID,
			models.InterbankNegotiationRoleSeller, localID,
		)
	default:
		return nil, errors.New("unknown role filter")
	}
	if !includeClosed {
		query = query.Where("is_ongoing = ?", true)
	}
	var negs []models.InterbankOtcNegotiation
	if err := query.Order("updated_at DESC, id DESC").Find(&negs).Error; err != nil {
		return nil, err
	}
	return negs, nil
}
