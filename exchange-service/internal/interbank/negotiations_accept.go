package interbank

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/RAF-SI-2025/EXBanka-3-Backend/exchange-service/internal/models"
)

// accept handles GET /negotiations/{routingNumber}/{id}/accept.
// Per spec §3.6 this is a GET that mutates state — accepting an
// open negotiation closes it and triggers an outbound NEW_TX from
// the seller's bank to the buyer's bank carrying the four postings
// that move the premium and create the option contract.
//
// Only the seller's bank can accept (this mirrors the existing
// local OTC-5 rule). Both NEW_TX and a follow-up COMMIT_TX are
// dispatched here; on a NO vote we send ROLLBACK_TX and reopen
// the negotiation so the participants can keep haggling.
func (h *NegotiationsHandler) accept(w http.ResponseWriter, r *http.Request, routing RoutingNumber, id string) {
	partner := PartnerFromContext(r.Context())
	if partner == nil {
		writeProblemJSON(w, http.StatusUnauthorized, "no partner in context")
		return
	}

	neg, err := h.repo.Get(int(routing), id)
	if err != nil {
		writeProblemJSON(w, http.StatusInternalServerError, fmt.Sprintf("loading negotiation: %v", err))
		return
	}
	if neg == nil {
		writeProblemJSON(w, http.StatusNotFound, "no such negotiation")
		return
	}
	if !partnerMayAccess(neg, partner) {
		writeProblemJSON(w, http.StatusForbidden, "this X-Api-Key is not a party to that negotiation")
		return
	}
	if !neg.IsOngoing {
		writeProblemJSON(w, http.StatusConflict, "negotiation is no longer ongoing")
		return
	}
	if neg.LocalRole != models.InterbankNegotiationRoleSeller {
		writeProblemJSON(w, http.StatusForbidden,
			"only the seller's bank may accept — the buyer's bank cannot self-accept")
		return
	}

	// Close locally first so a concurrent second accept can't double-spend.
	if err := h.repo.MarkClosed(int(routing), id); err != nil {
		writeProblemJSON(w, http.StatusInternalServerError, fmt.Sprintf("closing negotiation: %v", err))
		return
	}

	tx := buildOptionAcceptanceTx(h.registry.OwnRoutingNumber(), neg)

	txKey := h.client.NewIdempotenceKey()
	buyerCode := RoutingNumber(neg.BuyerRoutingNumber)

	vote, err := h.client.SendNewTx(r.Context(), buyerCode, txKey, &tx)
	if err != nil {
		// Network or partner error — reopen the negotiation so the
		// seller can decide whether to retry from the UI.
		slog.Error("interbank: NEW_TX dispatch failed during /accept",
			"err", err, "negotiation", id, "buyer", buyerCode)
		h.reopenAfterDispatchFailure(int(routing), id, neg.LastModifiedByRoutingNumber, neg.LastModifiedByID)
		writeProblemJSON(w, http.StatusBadGateway, fmt.Sprintf("dispatching NEW_TX: %v", err))
		return
	}

	if vote.Vote != VoteYes {
		// Buyer's bank refused — the negotiation should reopen so the
		// participants can keep going. We don't send ROLLBACK_TX
		// because NEW_TX with vote=NO is itself the rollback; the
		// buyer's bank holds no resources after a NO.
		slog.Info("interbank: NEW_TX received NO vote during /accept",
			"negotiation", id, "buyer", buyerCode, "reasons", vote.Reasons)
		h.reopenAfterDispatchFailure(int(routing), id, neg.LastModifiedByRoutingNumber, neg.LastModifiedByID)
		writeJSON(w, http.StatusConflict, vote)
		return
	}

	// YES vote — commit. If COMMIT_TX itself fails after retries,
	// we surface the error but leave the negotiation closed: the
	// buyer's bank has already voted to hold the resources, so the
	// resolution is operator-driven (CHECK_STATUS / replay), not
	// reopening the negotiation.
	commitKey := h.client.NewIdempotenceKey()
	if err := h.client.SendCommitTx(r.Context(), buyerCode, commitKey, tx.TransactionID); err != nil {
		slog.Error("interbank: COMMIT_TX dispatch failed after YES vote",
			"err", err, "negotiation", id, "transaction", tx.TransactionID.ID, "buyer", buyerCode)
		writeProblemJSON(w, http.StatusBadGateway,
			fmt.Sprintf("buyer voted YES but COMMIT_TX failed; operator action required: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, vote)
}

// buildOptionAcceptanceTx builds the protocol Transaction that
// /accept dispatches: four postings expressing the premium transfer
// (cash leg) and option-contract creation (option leg).
//
//	cash leg:    buyer -P   premium currency      → seller +P
//	option leg:  seller -1  OPTION{neg, stock, …} → buyer  +1
//
// The TransactionID is owned by the seller's bank (= us) since
// we initiated the NEW_TX. The buyer's bank stores it on receipt.
func buildOptionAcceptanceTx(ownRouting RoutingNumber, neg *models.InterbankOtcNegotiation) Transaction {
	buyer := TxAccount{
		Type: TxAccountPerson,
		ID: &ForeignBankId{
			RoutingNumber: RoutingNumber(neg.BuyerRoutingNumber),
			ID:            neg.BuyerID,
		},
	}
	seller := TxAccount{
		Type: TxAccountPerson,
		ID: &ForeignBankId{
			RoutingNumber: RoutingNumber(neg.SellerRoutingNumber),
			ID:            neg.SellerID,
		},
	}

	premiumAsset := Asset{
		Type:  AssetMonas,
		Monas: &MonetaryAsset{Currency: CurrencyCode(neg.PremiumCurrency)},
	}

	optionAsset := Asset{
		Type: AssetOption,
		Option: &OptionDescription{
			NegotiationID: ForeignBankId{
				RoutingNumber: RoutingNumber(neg.NegotiationRoutingNumber),
				ID:            neg.NegotiationID,
			},
			Stock: StockDescription{Ticker: neg.StockTicker},
			PricePerUnit: MonetaryValue{
				Currency: CurrencyCode(neg.PricePerUnitCurrency),
				Amount:   neg.PricePerUnitAmount,
			},
			SettlementDate: neg.SettlementDate,
			Amount:         neg.Amount,
		},
	}

	return Transaction{
		Postings: []Posting{
			{Account: buyer, Amount: -neg.PremiumAmount, Asset: premiumAsset},
			{Account: seller, Amount: neg.PremiumAmount, Asset: premiumAsset},
			{Account: seller, Amount: -1, Asset: optionAsset},
			{Account: buyer, Amount: 1, Asset: optionAsset},
		},
		TransactionID: ForeignBankId{
			RoutingNumber: ownRouting,
			ID:            uuid.NewString(),
		},
		Message:        fmt.Sprintf("OTC option acceptance for negotiation %s", neg.NegotiationID),
		PaymentCode:    "OTC",
		PaymentPurpose: "OTC option contract premium + creation",
	}
}

// reopenAfterDispatchFailure flips IsOngoing back to true so the
// participants can resume the negotiation. We don't roll back to a
// prior offer state — the most recent terms stay on record, just
// re-marked open.
func (h *NegotiationsHandler) reopenAfterDispatchFailure(routing int, id string, lastModRouting int, lastModID string) {
	// Reuse UpdateTerms to bump updated_at; the LastModifiedBy lands back
	// on whoever it was before so the wire copy looks unchanged from
	// the participants' perspective.
	if err := h.repo.MarkOngoing(routing, id, lastModRouting, lastModID); err != nil {
		slog.Error("interbank: reopening negotiation after dispatch failure",
			"err", err, "negotiation", id)
	}
}
