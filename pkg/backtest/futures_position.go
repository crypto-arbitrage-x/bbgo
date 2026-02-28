package backtest

import (
	"fmt"
	"time"

	"github.com/c9s/bbgo/pkg/fixedpoint"
)

var (
	defaultLeverage              = fixedpoint.NewFromInt(1)
	defaultMaintenanceMarginRate = fixedpoint.MustNewFromString("0.004") // 0.4%
	defaultFundingRate           = fixedpoint.MustNewFromString("0.0001") // 0.01%
)

// FuturesPositionTracker tracks the position of a single symbol in futures mode.
// Base > 0 means long, Base < 0 means short, Base == 0 means no position.
type FuturesPositionTracker struct {
	Symbol      string
	Base        fixedpoint.Value // positive = long, negative = short
	AverageCost fixedpoint.Value // weighted average entry price
	Quote       fixedpoint.Value // accumulated realized PnL (positive = profit)

	Leverage              fixedpoint.Value
	MaintenanceMarginRate fixedpoint.Value

	// margin locked for the current position
	LockedMargin fixedpoint.Value

	// FundingRate per 8 hours
	FundingRate      fixedpoint.Value
	LastFundingTime  time.Time
}

func NewFuturesPositionTracker(symbol string, leverage, maintenanceMarginRate, fundingRate fixedpoint.Value) *FuturesPositionTracker {
	if leverage.IsZero() {
		leverage = defaultLeverage
	}
	if maintenanceMarginRate.IsZero() {
		maintenanceMarginRate = defaultMaintenanceMarginRate
	}
	if fundingRate.IsZero() {
		fundingRate = defaultFundingRate
	}
	return &FuturesPositionTracker{
		Symbol:                symbol,
		Base:                  fixedpoint.Zero,
		AverageCost:           fixedpoint.Zero,
		Quote:                 fixedpoint.Zero,
		Leverage:              leverage,
		MaintenanceMarginRate: maintenanceMarginRate,
		LockedMargin:          fixedpoint.Zero,
		FundingRate:           fundingRate,
	}
}

// InitialMarginForNotional returns the margin required to open a position with the given notional value.
func (p *FuturesPositionTracker) InitialMarginForNotional(notional fixedpoint.Value) fixedpoint.Value {
	return notional.Div(p.Leverage)
}

// PositionNotional returns the current position notional at the given mark price.
func (p *FuturesPositionTracker) PositionNotional(markPrice fixedpoint.Value) fixedpoint.Value {
	return p.Base.Abs().Mul(markPrice)
}

// MaintenanceMargin returns the maintenance margin required for the current position.
func (p *FuturesPositionTracker) MaintenanceMargin(markPrice fixedpoint.Value) fixedpoint.Value {
	return p.PositionNotional(markPrice).Mul(p.MaintenanceMarginRate)
}

// UnrealizedPnL returns the unrealized PnL at the given mark price.
func (p *FuturesPositionTracker) UnrealizedPnL(markPrice fixedpoint.Value) fixedpoint.Value {
	if p.Base.IsZero() {
		return fixedpoint.Zero
	}
	// long: (markPrice - avgCost) * base
	// short: (avgCost - markPrice) * |base|  => same formula since base is negative
	return markPrice.Sub(p.AverageCost).Mul(p.Base)
}

// IsLiquidated checks if the position should be liquidated at the given mark price.
// Liquidation when: walletBalance + unrealizedPnL <= maintenanceMargin
func (p *FuturesPositionTracker) IsLiquidated(walletBalance, markPrice fixedpoint.Value) bool {
	if p.Base.IsZero() {
		return false
	}
	marginBalance := walletBalance.Add(p.UnrealizedPnL(markPrice))
	maintMargin := p.MaintenanceMargin(markPrice)
	return marginBalance.Compare(maintMargin) <= 0
}

// ProcessTrade processes a trade and returns (realizedPnL, marginDelta).
// marginDelta > 0 means margin is released, < 0 means margin is consumed.
func (p *FuturesPositionTracker) ProcessTrade(side string, quantity, price fixedpoint.Value) (realizedPnL, marginDelta fixedpoint.Value, err error) {
	realizedPnL = fixedpoint.Zero
	marginDelta = fixedpoint.Zero

	// Convert to signed quantity: buy = positive, sell = negative
	var signedQty fixedpoint.Value
	if side == "BUY" {
		signedQty = quantity
	} else {
		signedQty = quantity.Neg()
	}

	if p.Base.IsZero() {
		// Opening a new position
		notional := quantity.Mul(price)
		requiredMargin := p.InitialMarginForNotional(notional)
		p.Base = signedQty
		p.AverageCost = price
		p.LockedMargin = requiredMargin
		marginDelta = requiredMargin.Neg() // consuming margin
		return realizedPnL, marginDelta, nil
	}

	// Same direction: adding to position
	if (p.Base.Sign() > 0 && signedQty.Sign() > 0) || (p.Base.Sign() < 0 && signedQty.Sign() < 0) {
		// Update average cost with weighted average
		oldNotional := p.Base.Abs().Mul(p.AverageCost)
		addNotional := quantity.Mul(price)
		newBase := p.Base.Add(signedQty)
		p.AverageCost = oldNotional.Add(addNotional).Div(newBase.Abs())
		p.Base = newBase

		// Additional margin required
		additionalMargin := p.InitialMarginForNotional(addNotional)
		p.LockedMargin = p.LockedMargin.Add(additionalMargin)
		marginDelta = additionalMargin.Neg()
		return realizedPnL, marginDelta, nil
	}

	// Opposite direction: reducing or reversing position
	absBase := p.Base.Abs()
	if quantity.Compare(absBase) <= 0 {
		// Partial or full close
		// realized PnL = (price - avgCost) * closedQty (for long)
		// realized PnL = (avgCost - price) * closedQty (for short)
		if p.Base.Sign() > 0 {
			realizedPnL = price.Sub(p.AverageCost).Mul(quantity)
		} else {
			realizedPnL = p.AverageCost.Sub(price).Mul(quantity)
		}
		p.Quote = p.Quote.Add(realizedPnL)

		// Release proportional margin
		closeRatio := quantity.Div(absBase)
		releasedMargin := p.LockedMargin.Mul(closeRatio)

		newBase := p.Base.Add(signedQty)
		if newBase.Abs().Compare(fixedpoint.MustNewFromString("0.00000001")) < 0 {
			newBase = fixedpoint.Zero
			releasedMargin = p.LockedMargin // release all margin
		}

		p.Base = newBase
		p.LockedMargin = p.LockedMargin.Sub(releasedMargin)
		marginDelta = releasedMargin // releasing margin

		if p.Base.IsZero() {
			p.AverageCost = fixedpoint.Zero
			p.LockedMargin = fixedpoint.Zero
		}
		return realizedPnL, marginDelta, nil
	}

	// Reverse position: close current + open opposite
	// Step 1: close entire current position
	if p.Base.Sign() > 0 {
		realizedPnL = price.Sub(p.AverageCost).Mul(absBase)
	} else {
		realizedPnL = p.AverageCost.Sub(price).Mul(absBase)
	}
	p.Quote = p.Quote.Add(realizedPnL)

	// Release all current margin
	releasedMargin := p.LockedMargin

	// Step 2: open new position in opposite direction
	remainingQty := quantity.Sub(absBase)
	newNotional := remainingQty.Mul(price)
	requiredMargin := p.InitialMarginForNotional(newNotional)

	if side == "BUY" {
		p.Base = remainingQty
	} else {
		p.Base = remainingQty.Neg()
	}
	p.AverageCost = price
	p.LockedMargin = requiredMargin

	marginDelta = releasedMargin.Sub(requiredMargin) // net margin change
	return realizedPnL, marginDelta, nil
}

// IsReducingPosition checks if the given order side would reduce the current position.
func (p *FuturesPositionTracker) IsReducingPosition(side string, quantity fixedpoint.Value) bool {
	if p.Base.IsZero() {
		return false
	}
	// Long position: sell reduces
	if p.Base.Sign() > 0 && side == "SELL" && quantity.Compare(p.Base.Abs()) <= 0 {
		return true
	}
	// Short position: buy reduces
	if p.Base.Sign() < 0 && side == "BUY" && quantity.Compare(p.Base.Abs()) <= 0 {
		return true
	}
	return false
}

// CalculateFundingFee calculates the funding fee for the current position.
// Positive return means the position holder pays; negative means they receive.
func (p *FuturesPositionTracker) CalculateFundingFee(markPrice fixedpoint.Value) fixedpoint.Value {
	if p.Base.IsZero() {
		return fixedpoint.Zero
	}
	// funding fee = position notional * funding rate
	// long pays positive rate, short receives positive rate
	notional := p.PositionNotional(markPrice)
	fee := notional.Mul(p.FundingRate)
	if p.Base.Sign() > 0 {
		return fee // long pays
	}
	return fee.Neg() // short receives
}

// ForceClose calculates the realized PnL and margin release for a forced liquidation.
func (p *FuturesPositionTracker) ForceClose(price fixedpoint.Value) (realizedPnL fixedpoint.Value) {
	if p.Base.IsZero() {
		return fixedpoint.Zero
	}
	absBase := p.Base.Abs()
	if p.Base.Sign() > 0 {
		realizedPnL = price.Sub(p.AverageCost).Mul(absBase)
	} else {
		realizedPnL = p.AverageCost.Sub(price).Mul(absBase)
	}
	p.Quote = p.Quote.Add(realizedPnL)
	p.Base = fixedpoint.Zero
	p.AverageCost = fixedpoint.Zero
	p.LockedMargin = fixedpoint.Zero
	return realizedPnL
}

func (p *FuturesPositionTracker) String() string {
	return fmt.Sprintf("FuturesPosition{symbol=%s, base=%s, avgCost=%s, pnl=%s, margin=%s}",
		p.Symbol, p.Base.String(), p.AverageCost.String(), p.Quote.String(), p.LockedMargin.String())
}
