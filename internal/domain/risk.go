package domain

import (
	"time"

	"github.com/udaykishore-resu/tcrp/internal/money"
)

// CollateralLine is one instrument's contribution to collateral value.
type CollateralLine struct {
	Symbol      string       `json:"symbol"`
	Class       AssetClass   `json:"class"`
	Quantity    int64        `json:"quantity"`
	Price       money.Amount `json:"price"`
	Market      money.Amount `json:"market_value"`
	Haircut     money.Ratio  `json:"haircut"`
	AdvanceRate money.Ratio  `json:"advance_rate"`
	Collateral  money.Amount `json:"collateral_value"`
	Eligible    bool         `json:"eligible"`
	Ledger      Ledger       `json:"ledger"`
}

// CollateralValue is the haircut-adjusted value of a counterparty's pledge.
type CollateralValue struct {
	Counterparty    string           `json:"counterparty"`
	AsOf            time.Time        `json:"as_of"`
	Lines           []CollateralLine `json:"lines"`
	MarketValue     money.Amount     `json:"market_value"`
	CollateralValue money.Amount     `json:"collateral_value"`
	IneligibleValue money.Amount     `json:"ineligible_value"`
	Currency        money.Currency   `json:"currency"`
	HaircutVersion  string           `json:"haircut_version"`
	PriceAsOf       time.Time        `json:"price_as_of"`
}

// Exposure is what the institution stands to lose to a counterparty.
type Exposure struct {
	Counterparty  string         `json:"counterparty"`
	AsOf          time.Time      `json:"as_of"`
	GrossExposure money.Amount   `json:"gross_exposure"`
	NetExposure   money.Amount   `json:"net_exposure"`
	Collateral    money.Amount   `json:"collateral_value"`
	Uncovered     money.Amount   `json:"uncovered_exposure"`
	Shortfall     money.Amount   `json:"margin_shortfall"`
	Excess        money.Amount   `json:"margin_excess"`
	NettingSets   []NettingSet   `json:"netting_sets,omitempty"`
	Currency      money.Currency `json:"currency"`
	Covered       bool           `json:"covered"`
	// Threshold is the unsecured amount the agreement permits before a call.
	Threshold money.Amount `json:"threshold"`
	// MinimumTransfer suppresses calls below an operationally sensible size.
	MinimumTransfer money.Amount `json:"minimum_transfer"`
}

// NettingSet is a group of obligations that offset under one agreement.
type NettingSet struct {
	ID       string       `json:"id"`
	Gross    money.Amount `json:"gross"`
	Net      money.Amount `json:"net"`
	Count    int          `json:"count"`
	Enforced bool         `json:"enforceable"`
}

// MarginCallStatus is the lifecycle of a call.
type MarginCallStatus string

// Margin call statuses.
const (
	MarginProposed MarginCallStatus = "proposed"
	MarginIssued   MarginCallStatus = "issued"
	MarginMet      MarginCallStatus = "met"
	MarginDisputed MarginCallStatus = "disputed"
	MarginCancel   MarginCallStatus = "cancelled"
)

// MarginCall is a demand for additional collateral.
type MarginCall struct {
	ID           string           `json:"id"`
	Counterparty string           `json:"counterparty"`
	Amount       money.Amount     `json:"amount"`
	Status       MarginCallStatus `json:"status"`
	Basis        string           `json:"basis"`
	ExposureID   string           `json:"exposure_id,omitempty"`
	ProposedAt   time.Time        `json:"proposed_at"`
	IssuedAt     time.Time        `json:"issued_at,omitzero"`
	DueAt        time.Time        `json:"due_at,omitzero"`
	ApprovalID   string           `json:"approval_id,omitempty"`
	IssuedBy     string           `json:"issued_by,omitempty"`
}

// LimitKind classifies a risk limit.
type LimitKind string

// Limit kinds.
const (
	LimitCounterparty  LimitKind = "counterparty_exposure"
	LimitIssuerConc    LimitKind = "issuer_concentration"
	LimitClassConc     LimitKind = "asset_class_concentration"
	LimitSectorConc    LimitKind = "sector_concentration"
	LimitLedgerConc    LimitKind = "ledger_concentration"
	LimitUncoveredAggr LimitKind = "aggregate_uncovered"
)

// Limit is a risk appetite statement the desk is measured against.
type Limit struct {
	ID        string       `json:"id"`
	Kind      LimitKind    `json:"kind"`
	Scope     string       `json:"scope"`
	Amount    money.Amount `json:"amount,omitzero"`
	Ratio     money.Ratio  `json:"ratio,omitzero"`
	WarnAt    money.Ratio  `json:"warn_at"`
	Owner     string       `json:"owner"`
	Effective time.Time    `json:"effective"`
}

// BreachSeverity orders breaches.
type BreachSeverity string

// Breach severities.
const (
	BreachWarn     BreachSeverity = "warning"
	BreachHard     BreachSeverity = "breach"
	BreachCritical BreachSeverity = "critical"
)

// BreachStatus is the workflow state of a breach.
type BreachStatus string

// Breach statuses.
const (
	BreachOpen         BreachStatus = "open"
	BreachAcknowledged BreachStatus = "acknowledged"
	BreachRemediated   BreachStatus = "remediated"
	BreachAccepted     BreachStatus = "accepted"
)

// Breach is a limit exceeded.
type Breach struct {
	ID         string         `json:"id"`
	LimitID    string         `json:"limit_id"`
	Kind       LimitKind      `json:"kind"`
	Scope      string         `json:"scope"`
	Observed   money.Amount   `json:"observed,omitzero"`
	Limit      money.Amount   `json:"limit,omitzero"`
	ObservedPc money.Ratio    `json:"observed_pct,omitzero"`
	LimitPc    money.Ratio    `json:"limit_pct,omitzero"`
	Severity   BreachSeverity `json:"severity"`
	Status     BreachStatus   `json:"status"`
	DetectedAt time.Time      `json:"detected_at"`
	AsOf       time.Time      `json:"as_of"`
	TraceID    string         `json:"trace_id,omitempty"`
	Notes      []string       `json:"notes,omitempty"`
}

// Concentration is one slice of the portfolio measured against a limit.
type Concentration struct {
	Dimension string       `json:"dimension"`
	Bucket    string       `json:"bucket"`
	Value     money.Amount `json:"value"`
	Share     money.Ratio  `json:"share"`
	LimitPc   money.Ratio  `json:"limit_pct,omitzero"`
	Breached  bool         `json:"breached"`
}

// StressScenario is a named set of shocks.
type StressScenario struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// PriceShock is applied to every instrument, before class overrides.
	PriceShock money.Ratio `json:"price_shock"`
	// ClassShocks override PriceShock per asset class.
	ClassShocks map[AssetClass]money.Ratio `json:"class_shocks,omitempty"`
	// HaircutAddOn widens every haircut by this amount.
	HaircutAddOn money.Ratio `json:"haircut_add_on"`
	// DefaultedCounterparties are assumed to fail entirely.
	DefaultedCounterparties []string `json:"defaulted_counterparties,omitempty"`
	// RedemptionRate is the share of the fund assumed to be redeemed at once.
	RedemptionRate money.Ratio `json:"redemption_rate,omitzero"`
}

// StressResult is one counterparty's outcome under one scenario.
type StressResult struct {
	Scenario       string       `json:"scenario"`
	Counterparty   string       `json:"counterparty"`
	BaseCollateral money.Amount `json:"base_collateral"`
	StressedValue  money.Amount `json:"stressed_collateral"`
	BaseUncovered  money.Amount `json:"base_uncovered"`
	StressedUncov  money.Amount `json:"stressed_uncovered"`
	Delta          money.Amount `json:"delta"`
	NewShortfall   money.Amount `json:"new_shortfall"`
	BreachesLimit  bool         `json:"breaches_limit"`
}

// StressSummary aggregates a scenario run.
type StressSummary struct {
	Scenario       string         `json:"scenario"`
	AsOf           time.Time      `json:"as_of"`
	Results        []StressResult `json:"results"`
	TotalBase      money.Amount   `json:"total_base_collateral"`
	TotalStressed  money.Amount   `json:"total_stressed_collateral"`
	TotalShortfall money.Amount   `json:"total_new_shortfall"`
	WorstCP        string         `json:"worst_counterparty"`
	DurationMS     float64        `json:"duration_ms"`
	Counterparties int            `json:"counterparties"`
}
