package domain

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/money"
)

// Seed is a complete, deterministic dataset: the same inputs every time, so a
// demo, a test and an eval scenario all reason about the same portfolio and a
// number printed in the README can be checked by running the code.
type Seed struct {
	AsOf           time.Time
	Counterparties []Counterparty
	Instruments    []Instrument
	Prices         []Price
	Haircuts       HaircutSchedule
	Positions      []Position
	Obligations    []Obligation
	Limits         []Limit
	Scenarios      []StressScenario
	Events         []LedgerEvent
	// FundSymbol is the tokenised money-market fund share the platform issues.
	FundSymbol string
}

// FundSymbol is the tokenised money-market fund share.
const FundSymbol = "TMMF"

// SeedTime is the fixed as-of instant of the seeded world. A deterministic
// dataset needs a deterministic clock; every relative timestamp hangs off this.
var SeedTime = time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)

// NewSeed builds the dataset.
func NewSeed() *Seed {
	rng := rand.New(rand.NewSource(20260903))
	s := &Seed{AsOf: SeedTime, FundSymbol: FundSymbol}

	s.Counterparties = []Counterparty{
		{ID: "CP-ALPHA", Name: "Alpha Capital Markets", LEI: "5493001KJTIIGC8Y1R12", Jurisdiction: "US", Rating: "AA", Sector: "broker_dealer", Netting: true, Whitelisted: true, Wallets: []string{"0xa1f0000000000000000000000000000000000001"}, Parties: []string{"Alpha::1220"}},
		{ID: "CP-BRAVO", Name: "Bravo Pensions Trust", LEI: "5493001KJTIIGC8Y1R13", Jurisdiction: "GB", Rating: "AAA", Sector: "pension", Netting: true, Whitelisted: true, Wallets: []string{"0xa1f0000000000000000000000000000000000002"}, Parties: []string{"Bravo::1220"}},
		{ID: "CP-CHARLIE", Name: "Charlie Asset Management", LEI: "5493001KJTIIGC8Y1R14", Jurisdiction: "LU", Rating: "A", Sector: "asset_manager", Netting: true, Whitelisted: true, Wallets: []string{"0xa1f0000000000000000000000000000000000003"}, Parties: []string{"Charlie::1220"}},
		{ID: "CP-DELTA", Name: "Delta Structured Finance", LEI: "5493001KJTIIGC8Y1R15", Jurisdiction: "US", Rating: "BBB", Sector: "structured", Netting: true, Whitelisted: true, Wallets: []string{"0xa1f0000000000000000000000000000000000004"}, Parties: []string{"Delta::1220"}},
		{ID: "CP-ECHO", Name: "Echo Regional Bank", LEI: "5493001KJTIIGC8Y1R16", Jurisdiction: "DE", Rating: "A", Sector: "bank", Netting: false, Whitelisted: true, Wallets: []string{"0xa1f0000000000000000000000000000000000005"}, Parties: []string{"Echo::1220"}},
		{ID: "CP-FOXTROT", Name: "Foxtrot Hedge Partners", LEI: "5493001KJTIIGC8Y1R17", Jurisdiction: "KY", Rating: "BB", Sector: "hedge_fund", Netting: true, Whitelisted: true, Wallets: []string{"0xa1f0000000000000000000000000000000000006"}, Parties: []string{"Foxtrot::1220"}},
		{ID: "CP-GOLF", Name: "Golf Insurance Group", LEI: "5493001KJTIIGC8Y1R18", Jurisdiction: "FR", Rating: "AA", Sector: "insurance", Netting: true, Whitelisted: true, Wallets: []string{"0xa1f0000000000000000000000000000000000007"}, Parties: []string{"Golf::1220"}},
		// Not in the identity registry: every transfer to this counterparty
		// must be refused before it reaches a ledger.
		{ID: "CP-HOTEL", Name: "Hotel Offshore Ltd", LEI: "5493001KJTIIGC8Y1R19", Jurisdiction: "VG", Rating: "B", Sector: "other", Netting: false, Whitelisted: false, Wallets: []string{"0xa1f0000000000000000000000000000000000008"}},
	}

	s.Instruments = []Instrument{
		{Symbol: FundSymbol, Name: "Tokenised USD Money Market Fund Share", Issuer: "SELF", Sector: "fund", Class: ClassMMF, Currency: money.USD, Rating: "AAA", Tokenised: true},
		{Symbol: "UST-2Y", Name: "US Treasury 2Y", Issuer: "US-TREASURY", Sector: "sovereign", Class: ClassGovBond, Currency: money.USD, Rating: "AAA"},
		{Symbol: "UST-10Y", Name: "US Treasury 10Y", Issuer: "US-TREASURY", Sector: "sovereign", Class: ClassGovBond, Currency: money.USD, Rating: "AAA"},
		{Symbol: "BUND-10Y", Name: "German Bund 10Y", Issuer: "DE-BUND", Sector: "sovereign", Class: ClassGovBond, Currency: money.EUR, Rating: "AAA"},
		{Symbol: "CORP-AAPL", Name: "Apple Inc 2031", Issuer: "AAPL", Sector: "technology", Class: ClassCorpBond, Currency: money.USD, Rating: "AA"},
		{Symbol: "CORP-JPM", Name: "JPMorgan 2029", Issuer: "JPM", Sector: "financials", Class: ClassCorpBond, Currency: money.USD, Rating: "A"},
		{Symbol: "CORP-XYZ", Name: "XYZ Industries 2028", Issuer: "XYZ", Sector: "industrials", Class: ClassCorpBond, Currency: money.USD, Rating: "BBB"},
		{Symbol: "EQ-SPX", Name: "S&P 500 ETF", Issuer: "SPX", Sector: "index", Class: ClassEquity, Currency: money.USD, Rating: ""},
		{Symbol: "USDC", Name: "USD Coin", Issuer: "CIRCLE", Sector: "stablecoin", Class: ClassStableCoin, Currency: money.USD, Rating: "", Tokenised: true},
		{Symbol: "CASH-USD", Name: "USD Cash", Issuer: "SELF", Sector: "cash", Class: ClassCash, Currency: money.USD, Rating: "AAA"},
	}

	px := map[string]string{
		FundSymbol: "1.0000", "UST-2Y": "98.7500", "UST-10Y": "95.2500", "BUND-10Y": "99.1000",
		"CORP-AAPL": "101.3750", "CORP-JPM": "97.8750", "CORP-XYZ": "88.5000",
		"EQ-SPX": "512.4000", "USDC": "1.0000", "CASH-USD": "1.0000",
	}
	for _, i := range s.Instruments {
		s.Prices = append(s.Prices, Price{
			Symbol: i.Symbol, Px: money.MustParse(i.Currency, px[i.Symbol]),
			AsOf: SeedTime.Add(-5 * time.Minute), Source: "marks/eod",
		})
	}

	s.Haircuts = HaircutSchedule{
		Version: "haircuts-2026.09",
		Rules: []HaircutRule{
			{Class: ClassCash, Haircut: money.Bps(0), Eligible: true},
			{Class: ClassMMF, Rating: "AAA", Haircut: money.Bps(50), Eligible: true},
			{Class: ClassMMF, Haircut: money.Bps(150), Eligible: true},
			{Class: ClassGovBond, Rating: "AAA", Haircut: money.Bps(200), Eligible: true},
			{Class: ClassGovBond, Haircut: money.Bps(400), Eligible: true},
			{Class: ClassCorpBond, Rating: "AA", Haircut: money.Bps(500), Eligible: true},
			{Class: ClassCorpBond, Rating: "A", Haircut: money.Bps(700), Eligible: true},
			{Class: ClassCorpBond, Rating: "BBB", Haircut: money.Bps(1200), Eligible: true},
			{Class: ClassCorpBond, Haircut: money.Bps(2500), Eligible: true},
			{Class: ClassStableCoin, Haircut: money.Bps(300), Eligible: true},
			{Class: ClassEquity, Haircut: money.Bps(1500), Eligible: true},
		},
	}

	// Positions. The distribution is hand-set rather than random so that the
	// interesting cases — an undermargined counterparty, a concentration
	// breach, a ledger break — exist by construction and can be asserted on.
	type holding struct {
		cp     string
		symbol string
		qty    int64
		ledger Ledger
	}
	holdings := []holding{
		{"CP-ALPHA", FundSymbol, 40_000_000, LedgerEVM},
		{"CP-ALPHA", "UST-10Y", 120_000, LedgerGolden},
		{"CP-ALPHA", "CORP-JPM", 90_000, LedgerGolden},
		{"CP-BRAVO", FundSymbol, 65_000_000, LedgerCanton},
		{"CP-BRAVO", "UST-2Y", 250_000, LedgerGolden},
		{"CP-BRAVO", "BUND-10Y", 80_000, LedgerGolden},
		{"CP-CHARLIE", FundSymbol, 22_000_000, LedgerEVM},
		{"CP-CHARLIE", "EQ-SPX", 30_000, LedgerGolden},
		{"CP-CHARLIE", "CORP-AAPL", 60_000, LedgerGolden},
		// Delta is thinly collateralised against a large repo book.
		{"CP-DELTA", FundSymbol, 8_000_000, LedgerEVM},
		{"CP-DELTA", "CORP-XYZ", 45_000, LedgerGolden},
		{"CP-ECHO", FundSymbol, 31_000_000, LedgerCanton},
		{"CP-ECHO", "USDC", 5_000_000, LedgerEVM},
		{"CP-FOXTROT", FundSymbol, 12_500_000, LedgerEVM},
		{"CP-FOXTROT", "EQ-SPX", 22_000, LedgerGolden},
		{"CP-FOXTROT", "CORP-XYZ", 70_000, LedgerGolden},
		{"CP-GOLF", FundSymbol, 54_000_000, LedgerCanton},
		{"CP-GOLF", "UST-10Y", 210_000, LedgerGolden},
		{"CP-GOLF", "CASH-USD", 15_000_000, LedgerGolden},
	}
	for i, h := range holdings {
		s.Positions = append(s.Positions, Position{
			ID:           fmt.Sprintf("POS-%04d", i+1),
			Counterparty: h.cp, Symbol: h.symbol, Ledger: h.ledger, Quantity: h.qty,
			Pledged:   true,
			ValidFrom: SeedTime.Add(-72 * time.Hour),
			TxFrom:    SeedTime.Add(-72 * time.Hour).Add(time.Duration(rng.Intn(600)) * time.Second),
			Source:    sourceFor(h.ledger),
		})
	}

	// Obligations: what each counterparty owes. Delta's repo book is large
	// relative to its pledge, which is what produces the margin call.
	type owed struct {
		cp     string
		kind   ObligationKind
		amount string
		set    string
	}
	owings := []owed{
		{"CP-ALPHA", ObligationRepo, "48000000.00", "NS-ALPHA-1"},
		{"CP-ALPHA", ObligationDerivative, "-6500000.00", "NS-ALPHA-1"},
		{"CP-BRAVO", ObligationRepo, "70000000.00", "NS-BRAVO-1"},
		{"CP-BRAVO", ObligationDerivative, "-12000000.00", "NS-BRAVO-1"},
		{"CP-CHARLIE", ObligationRepo, "36000000.00", "NS-CHARLIE-1"},
		{"CP-DELTA", ObligationRepo, "42000000.00", "NS-DELTA-1"},
		{"CP-DELTA", ObligationDerivative, "3500000.00", "NS-DELTA-1"},
		{"CP-ECHO", ObligationRepo, "29000000.00", "NS-ECHO-1"},
		{"CP-ECHO", ObligationLoan, "4000000.00", "NS-ECHO-2"},
		{"CP-FOXTROT", ObligationRepo, "26000000.00", "NS-FOX-1"},
		{"CP-FOXTROT", ObligationDerivative, "2750000.00", "NS-FOX-1"},
		{"CP-GOLF", ObligationRepo, "58000000.00", "NS-GOLF-1"},
	}
	for i, o := range owings {
		s.Obligations = append(s.Obligations, Obligation{
			ID: fmt.Sprintf("OBL-%04d", i+1), Counterparty: o.cp, Kind: o.kind,
			Amount: money.MustParse(money.USD, o.amount), NettingSet: o.set,
			AsOf: SeedTime, DueAt: SeedTime.Add(24 * time.Hour),
		})
	}

	s.Limits = []Limit{
		{ID: "LIM-CP-EXP", Kind: LimitCounterparty, Scope: "*", Amount: money.MustParse(money.USD, "20000000.00"), WarnAt: money.Percent(80), Owner: "risk-manager", Effective: SeedTime.Add(-30 * 24 * time.Hour)},
		{ID: "LIM-ISSUER-CONC", Kind: LimitIssuerConc, Scope: "*", Ratio: money.Percent(35), WarnAt: money.Percent(80), Owner: "risk-manager", Effective: SeedTime.Add(-30 * 24 * time.Hour)},
		{ID: "LIM-CLASS-CONC", Kind: LimitClassConc, Scope: string(ClassEquity), Ratio: money.Percent(15), WarnAt: money.Percent(80), Owner: "risk-manager", Effective: SeedTime.Add(-30 * 24 * time.Hour)},
		{ID: "LIM-LEDGER-CONC", Kind: LimitLedgerConc, Scope: string(LedgerEVM), Ratio: money.Percent(60), WarnAt: money.Percent(90), Owner: "treasurer", Effective: SeedTime.Add(-30 * 24 * time.Hour)},
		{ID: "LIM-AGG-UNCOV", Kind: LimitUncoveredAggr, Scope: "*", Amount: money.MustParse(money.USD, "75000000.00"), WarnAt: money.Percent(85), Owner: "treasurer", Effective: SeedTime.Add(-30 * 24 * time.Hour)},
	}

	s.Scenarios = []StressScenario{
		{
			ID: "rate_shock", Name: "Parallel rate shock +200bp",
			Description: "Bond prices fall with duration; the fund share is unaffected in NAV terms but its haircut widens.",
			PriceShock:  money.Bps(-100),
			ClassShocks: map[AssetClass]money.Ratio{
				ClassGovBond: money.Bps(-900), ClassCorpBond: money.Bps(-1200), ClassEquity: money.Bps(-700),
			},
			HaircutAddOn: money.Bps(100),
		},
		{
			ID: "haircut_widening", Name: "Haircut widening",
			Description:  "A funding stress in which every haircut widens by 500bp with no price move.",
			HaircutAddOn: money.Bps(500),
		},
		{
			ID: "counterparty_default", Name: "Largest counterparty default",
			Description:             "The largest single counterparty fails; its collateral is liquidated into a falling market.",
			PriceShock:              money.Bps(-500),
			DefaultedCounterparties: []string{"CP-BRAVO"},
			HaircutAddOn:            money.Bps(200),
		},
		{
			ID: "redemption_run", Name: "Fund redemption run",
			Description:    "A quarter of the fund is redeemed in one session; the share is marked below par and its haircut trebles.",
			ClassShocks:    map[AssetClass]money.Ratio{ClassMMF: money.Bps(-150), ClassStableCoin: money.Bps(-200)},
			HaircutAddOn:   money.Bps(300),
			RedemptionRate: money.Percent(25),
		},
	}

	s.Events = seedEvents(s, rng)
	return s
}

func sourceFor(l Ledger) string {
	switch l {
	case LedgerEVM:
		return "indexer/evm"
	case LedgerCanton:
		return "indexer/canton"
	default:
		return "book-of-record"
	}
}

// seedEvents builds the ledger event stream the lakehouse ingests. It contains
// three deliberate anomalies, because a reconciliation control with nothing to
// find proves nothing:
//
//   - a transfer observed on chain that never reached the book of record,
//   - a quantity that disagrees between the book and the chain,
//   - an event observed late, after the as-of cut, which a bitemporal store must
//     place at its occurrence time without rewriting what was known before.
func seedEvents(s *Seed, rng *rand.Rand) []LedgerEvent {
	var out []LedgerEvent
	seq := uint64(0)
	block := uint64(21_400_000)
	t := SeedTime.Add(-48 * time.Hour)

	add := func(e LedgerEvent) {
		seq++
		block++
		e.Seq = seq
		e.Block = block
		if e.TxHash == "" {
			e.TxHash = fmt.Sprintf("0x%064x", rng.Uint64())
		}
		if e.OccurredAt.IsZero() {
			e.OccurredAt = t
		}
		if e.ObservedAt.IsZero() {
			e.ObservedAt = e.OccurredAt.Add(time.Duration(2+rng.Intn(20)) * time.Second)
		}
		e.Finalised = true
		out = append(out, e)
		t = t.Add(time.Duration(3+rng.Intn(25)) * time.Minute)
	}

	// Issuance of the fund share to each holder.
	for _, p := range s.Positions {
		if p.Symbol != FundSymbol {
			continue
		}
		add(LedgerEvent{
			Ledger: p.Ledger, Kind: EventMint, Symbol: FundSymbol,
			To: p.Counterparty, Quantity: p.Quantity, Memo: "primary issuance",
		})
	}

	// Ordinary secondary activity.
	pairs := [][2]string{
		{"CP-ALPHA", "CP-CHARLIE"}, {"CP-GOLF", "CP-ECHO"}, {"CP-BRAVO", "CP-ALPHA"},
		{"CP-CHARLIE", "CP-FOXTROT"}, {"CP-ECHO", "CP-GOLF"},
	}
	for i, p := range pairs {
		add(LedgerEvent{
			Ledger: LedgerEVM, Kind: EventTransfer, Symbol: FundSymbol,
			From: p[0], To: p[1], Quantity: int64(250_000 * (i + 1)),
		})
	}

	// Anomaly 1: an on-chain transfer the book of record never received.
	add(LedgerEvent{
		Ledger: LedgerEVM, Kind: EventTransfer, Symbol: FundSymbol,
		From: "CP-FOXTROT", To: "CP-DELTA", Quantity: 1_750_000,
		Memo: "settlement instruction SI-88421",
	})

	// Anomaly 2: a partial fill the book recorded at a different size.
	add(LedgerEvent{
		Ledger: LedgerCanton, Kind: EventTransfer, Symbol: FundSymbol,
		From: "CP-GOLF", To: "CP-BRAVO", Quantity: 900_000,
		Memo: "partial fill; book shows 1,000,000",
	})

	// Anomaly 3: an event that occurred before the cut but was observed after
	// it, which is what a bitemporal store exists to handle correctly.
	late := LedgerEvent{
		Ledger: LedgerEVM, Kind: EventTransfer, Symbol: FundSymbol,
		From: "CP-ALPHA", To: "CP-ECHO", Quantity: 400_000,
		OccurredAt: SeedTime.Add(-90 * time.Minute),
		ObservedAt: SeedTime.Add(35 * time.Minute),
		Memo:       "late-arriving; observed after the risk cut",
	}
	add(late)

	// A bridge lock/mint pair, which the supply reconciliation must balance.
	add(LedgerEvent{
		Ledger: LedgerEVM, Kind: EventLock, Symbol: FundSymbol,
		From: "CP-BRAVO", Quantity: 5_000_000, Memo: "bridge lock nonce 1",
	})
	add(LedgerEvent{
		Ledger: LedgerCanton, Kind: EventMint, Symbol: FundSymbol,
		To: "CP-BRAVO", Quantity: 5_000_000, Memo: "bridge mint nonce 1",
	})

	SortEvents(out)
	return out
}

// Counterparty returns a counterparty by id.
func (s *Seed) Counterparty(id string) (Counterparty, bool) {
	for _, c := range s.Counterparties {
		if c.ID == id {
			return c, true
		}
	}
	return Counterparty{}, false
}

// Instrument returns an instrument by symbol.
func (s *Seed) Instrument(sym string) (Instrument, bool) {
	for _, i := range s.Instruments {
		if i.Symbol == sym {
			return i, true
		}
	}
	return Instrument{}, false
}

// Price returns the mark for a symbol.
func (s *Seed) Price(sym string) (Price, bool) {
	for _, p := range s.Prices {
		if p.Symbol == sym {
			return p, true
		}
	}
	return Price{}, false
}

// Scenario returns a stress scenario by id.
func (s *Seed) Scenario(id string) (StressScenario, bool) {
	for _, sc := range s.Scenarios {
		if sc.ID == id {
			return sc, true
		}
	}
	return StressScenario{}, false
}
