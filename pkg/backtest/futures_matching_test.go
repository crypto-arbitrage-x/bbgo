package backtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
)

func getTestFuturesAccount() *types.Account {
	account := &types.Account{
		MakerFeeRate: fixedpoint.NewFromFloat(0.02 * 0.01),  // 0.02%
		TakerFeeRate: fixedpoint.NewFromFloat(0.05 * 0.01),  // 0.05%
		AccountType:  types.AccountTypeFutures,
	}
	account.UpdateBalances(types.BalanceMap{
		"USDT": {Currency: "USDT", Available: fixedpoint.NewFromFloat(10000.0)},
	})
	return account
}

func newFuturesEngine(account *types.Account, market types.Market, leverage float64) *SimplePriceMatching {
	return &SimplePriceMatching{
		account:      account,
		Market:       market,
		currentTime:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		closedOrders: make(map[uint64]types.Order),
		lastPrice:    fixedpoint.NewFromFloat(20000.0),
		isFutures:    true,
		futuresPosition: NewFuturesPositionTracker(
			market.Symbol,
			fixedpoint.NewFromFloat(leverage),
			fixedpoint.Zero, // default maintenance margin rate
			fixedpoint.Zero, // default funding rate
		),
	}
}

func TestFutures_OpenLong(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Buy 0.1 BTC at market price 20000, with 10x leverage
	// Notional = 0.1 * 20000 = 2000 USDT
	// Margin required = 2000 / 10 = 200 USDT
	order, trade, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)
	assert.NotNil(t, order)
	assert.NotNil(t, trade)
	assert.True(t, trade.IsFutures)

	// Check position
	pos := engine.futuresPosition
	assert.Equal(t, "0.1", pos.Base.String())
	assert.Equal(t, "20000", pos.AverageCost.String())

	// Check balance: 10000 - 200 (margin) - fee
	bal, _ := account.Balance("USDT")
	// Fee = 0.1 * 20000 * 0.05% = 1 USDT
	// Available = 10000 - 200 - 1 = 9799
	assert.Equal(t, "9799", bal.Available.String())
}

func TestFutures_OpenShort(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Sell 0.1 BTC at market price 20000, with 10x leverage
	// This should NOT require any BTC balance — only USDT margin
	order, trade, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeSell,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)
	assert.NotNil(t, order)
	assert.NotNil(t, trade)

	// Check position: should be short
	pos := engine.futuresPosition
	assert.Equal(t, "-0.1", pos.Base.String())
	assert.Equal(t, "20000", pos.AverageCost.String())

	// Check balance: 10000 - 200 (margin) - 1 (fee)
	bal, _ := account.Balance("USDT")
	assert.Equal(t, "9799", bal.Available.String())
}

func TestFutures_OpenShort_NoBaseRequired(t *testing.T) {
	// This is the key test: in spot mode, selling without base would fail.
	// In futures mode, it should work with only USDT.
	account := &types.Account{
		MakerFeeRate: fixedpoint.NewFromFloat(0.02 * 0.01),
		TakerFeeRate: fixedpoint.NewFromFloat(0.05 * 0.01),
		AccountType:  types.AccountTypeFutures,
	}
	account.UpdateBalances(types.BalanceMap{
		"USDT": {Currency: "USDT", Available: fixedpoint.NewFromFloat(5000.0)},
		// No BTC balance at all!
	})

	market := getTestMarket()
	engine := newFuturesEngine(account, market, 5)
	engine.account = account

	order, trade, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeSell,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err, "short selling should work without BTC balance in futures mode")
	assert.NotNil(t, order)
	assert.NotNil(t, trade)

	pos := engine.futuresPosition
	assert.Equal(t, "-0.1", pos.Base.String())
}

func TestFutures_CloseLong_Profit(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Open long at 20000
	_, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	// Price goes up to 22000
	engine.lastPrice = fixedpoint.NewFromFloat(22000)

	// Close long by selling at 22000
	_, _, err = engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeSell,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	// Position should be flat
	pos := engine.futuresPosition
	assert.True(t, pos.Base.IsZero(), "position should be closed")

	// Realized PnL = (22000 - 20000) * 0.1 = 200 USDT
	assert.Equal(t, "200", pos.Quote.String())

	// Balance: 10000 - fee_open - fee_close + realized_pnl
	// fee_open = 0.1 * 20000 * 0.05% = 1
	// fee_close = 0.1 * 22000 * 0.05% = 1.1
	// Balance = 10000 - 1 - 1.1 + 200 = 10197.9
	bal, _ := account.Balance("USDT")
	expected := fixedpoint.NewFromFloat(10000).
		Sub(fixedpoint.NewFromFloat(1)).       // open fee
		Sub(fixedpoint.NewFromFloat(1.1)).     // close fee
		Add(fixedpoint.NewFromFloat(200))      // profit
	assert.Equal(t, expected.String(), bal.Available.String())
}

func TestFutures_CloseShort_Profit(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Open short at 20000
	_, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeSell,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	// Price goes down to 18000
	engine.lastPrice = fixedpoint.NewFromFloat(18000)

	// Close short by buying at 18000
	_, _, err = engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	// Position should be flat
	pos := engine.futuresPosition
	assert.True(t, pos.Base.IsZero())

	// Realized PnL = (20000 - 18000) * 0.1 = 200 USDT
	assert.Equal(t, "200", pos.Quote.String())
}

func TestFutures_CloseShort_Loss(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Open short at 20000
	_, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeSell,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	// Price goes up to 22000 (loss for short)
	engine.lastPrice = fixedpoint.NewFromFloat(22000)

	// Close short by buying at 22000
	_, _, err = engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	pos := engine.futuresPosition
	assert.True(t, pos.Base.IsZero())

	// Realized PnL = (20000 - 22000) * 0.1 = -200 USDT
	assert.Equal(t, "-200", pos.Quote.String())
}

func TestFutures_ReduceOnly(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Open long
	_, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	// ReduceOnly sell should work (reduces long)
	_, _, err = engine.PlaceOrder(types.SubmitOrder{
		Symbol:     "BTCUSDT",
		Side:       types.SideTypeSell,
		Type:       types.OrderTypeMarket,
		Quantity:   fixedpoint.NewFromFloat(0.05),
		ReduceOnly: true,
	})
	assert.NoError(t, err)

	// ReduceOnly buy should fail (would increase long)
	_, _, err = engine.PlaceOrder(types.SubmitOrder{
		Symbol:     "BTCUSDT",
		Side:       types.SideTypeBuy,
		Type:       types.OrderTypeMarket,
		Quantity:   fixedpoint.NewFromFloat(0.05),
		ReduceOnly: true,
	})
	assert.Error(t, err, "reduce-only buy should fail when holding a long position")

	// ReduceOnly sell with too much quantity should fail
	_, _, err = engine.PlaceOrder(types.SubmitOrder{
		Symbol:     "BTCUSDT",
		Side:       types.SideTypeSell,
		Type:       types.OrderTypeMarket,
		Quantity:   fixedpoint.NewFromFloat(0.1),  // only 0.05 left
		ReduceOnly: true,
	})
	assert.Error(t, err, "reduce-only with quantity exceeding position should fail")
}

func TestFutures_LimitOrder_MakerFill(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Place limit buy order below current price (maker)
	order, trade, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeLimit,
		Price:    fixedpoint.NewFromFloat(19000),
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)
	assert.NotNil(t, order)
	assert.Nil(t, trade, "limit order should not be filled immediately")

	// Process kline that dips to 18500
	k := newKLine("BTCUSDT", types.Interval1m,
		time.Date(2025, 1, 1, 0, 1, 0, 0, time.UTC),
		20000, 20000, 18500, 19500)
	engine.processKLine(k)

	// Order should be filled, position should be long
	pos := engine.futuresPosition
	assert.Equal(t, "0.1", pos.Base.String())
	assert.Equal(t, "19000", pos.AverageCost.String())
}

func TestFutures_LimitOrder_ShortMakerFill(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Place limit sell order above current price (maker)
	order, trade, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeSell,
		Type:     types.OrderTypeLimit,
		Price:    fixedpoint.NewFromFloat(21000),
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)
	assert.NotNil(t, order)
	assert.Nil(t, trade, "limit order should not be filled immediately")

	// Process kline that goes up to 21500
	k := newKLine("BTCUSDT", types.Interval1m,
		time.Date(2025, 1, 1, 0, 1, 0, 0, time.UTC),
		20000, 21500, 19500, 21000)
	engine.processKLine(k)

	// Position should be short
	pos := engine.futuresPosition
	assert.Equal(t, "-0.1", pos.Base.String())
	assert.Equal(t, "21000", pos.AverageCost.String())
}

func TestFutures_CancelOrder(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	balBefore, _ := account.Balance("USDT")

	// Place limit buy order
	order, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeLimit,
		Price:    fixedpoint.NewFromFloat(19000),
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	// Balance should have margin locked
	balAfterPlace, _ := account.Balance("USDT")
	// Margin = 0.1 * 19000 / 10 = 190
	expectedLocked := fixedpoint.NewFromFloat(190)
	assert.Equal(t, expectedLocked.String(), balAfterPlace.Locked.String())

	// Cancel order
	_, err = engine.CancelOrder(*order)
	assert.NoError(t, err)

	// Balance should be fully restored
	balAfterCancel, _ := account.Balance("USDT")
	assert.Equal(t, balBefore.Available.String(), balAfterCancel.Available.String())
	assert.True(t, balAfterCancel.Locked.IsZero())
}

func TestFutures_Liquidation(t *testing.T) {
	// Use small balance to trigger liquidation easily
	account := &types.Account{
		MakerFeeRate: fixedpoint.NewFromFloat(0.02 * 0.01),
		TakerFeeRate: fixedpoint.NewFromFloat(0.05 * 0.01),
		AccountType:  types.AccountTypeFutures,
	}
	account.UpdateBalances(types.BalanceMap{
		"USDT": {Currency: "USDT", Available: fixedpoint.NewFromFloat(250.0)},
	})

	market := getTestMarket()
	engine := &SimplePriceMatching{
		account:      account,
		Market:       market,
		currentTime:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		closedOrders: make(map[uint64]types.Order),
		lastPrice:    fixedpoint.NewFromFloat(20000.0),
		isFutures:    true,
		futuresPosition: NewFuturesPositionTracker(
			market.Symbol,
			fixedpoint.NewFromFloat(10),
			fixedpoint.MustNewFromString("0.01"), // 1% maintenance margin rate for easier testing
			fixedpoint.Zero,
		),
	}

	// Open long with 10x leverage
	// Margin = 0.1 * 20000 / 10 = 200 USDT
	_, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	// Price crashes — process a kline that dips very low
	// At low=17500, unrealized PnL = (17500-20000) * 0.1 = -250
	// Wallet balance = 250 - 1 (fee) = 249
	// Margin balance = 249 + (-250) = -1 < maintenance margin
	k := newKLine("BTCUSDT", types.Interval1m,
		time.Date(2025, 1, 1, 0, 1, 0, 0, time.UTC),
		20000, 20000, 17000, 18000)

	var gotLiqTrade bool
	engine.OnTradeUpdate(func(trade types.Trade) {
		if trade.IsFutures && trade.Side == types.SideTypeSell {
			gotLiqTrade = true
		}
	})

	engine.processKLine(k)

	// Position should be force-closed
	pos := engine.futuresPosition
	assert.True(t, pos.Base.IsZero(), "position should be liquidated")
	assert.True(t, gotLiqTrade, "should have emitted a liquidation trade")
}

func TestFutures_FundingFee(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Set funding rate to 0.01% per 8h
	engine.futuresPosition.FundingRate = fixedpoint.MustNewFromString("0.0001")

	// Open long position
	_, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)

	balAfterOpen, _ := account.Balance("USDT")

	// Process a kline that crosses the 08:00 UTC funding time.
	// newKLine sets EndTime = StartTime + interval - 1ms.
	// Use a 5min kline starting at 07:57, so EndTime = 08:01:59.999
	// This ensures the 08:00 funding boundary is within [StartTime, EndTime].
	k := newKLine("BTCUSDT", types.Interval5m,
		time.Date(2025, 1, 1, 7, 57, 0, 0, time.UTC),
		20000, 20100, 19900, 20000)
	engine.processKLine(k)

	balAfterFunding, _ := account.Balance("USDT")

	// Funding fee = notional * rate = (0.1 * 20000) * 0.0001 = 0.2 USDT
	// Long position pays positive funding rate
	expectedFee := fixedpoint.NewFromFloat(0.2)
	diff := balAfterOpen.Available.Sub(balAfterFunding.Available)
	assert.Equal(t, expectedFee.String(), diff.String(), "funding fee should be deducted from balance")
}

func TestFutures_InsufficientMargin(t *testing.T) {
	account := &types.Account{
		MakerFeeRate: fixedpoint.NewFromFloat(0.02 * 0.01),
		TakerFeeRate: fixedpoint.NewFromFloat(0.05 * 0.01),
		AccountType:  types.AccountTypeFutures,
	}
	account.UpdateBalances(types.BalanceMap{
		"USDT": {Currency: "USDT", Available: fixedpoint.NewFromFloat(100.0)},
	})

	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)
	engine.account = account

	// Try to open a position requiring 200 USDT margin with only 100 USDT
	// Margin = 0.1 * 20000 / 10 = 200 USDT
	_, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.Error(t, err, "should fail with insufficient margin")
}

func TestFutures_ReversePosition(t *testing.T) {
	account := getTestFuturesAccount()
	market := getTestMarket()
	engine := newFuturesEngine(account, market, 10)

	// Open long 0.1 BTC at 20000
	_, _, err := engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeBuy,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.1),
	})
	assert.NoError(t, err)
	assert.Equal(t, "0.1", engine.futuresPosition.Base.String())

	// Sell 0.15 BTC — close long 0.1 + open short 0.05
	engine.lastPrice = fixedpoint.NewFromFloat(21000)
	_, _, err = engine.PlaceOrder(types.SubmitOrder{
		Symbol:   "BTCUSDT",
		Side:     types.SideTypeSell,
		Type:     types.OrderTypeMarket,
		Quantity: fixedpoint.NewFromFloat(0.15),
	})
	assert.NoError(t, err)

	pos := engine.futuresPosition
	assert.Equal(t, "-0.05", pos.Base.String(), "should have a short position")
	assert.Equal(t, "21000", pos.AverageCost.String(), "new position should be at reversal price")

	// Realized PnL from closing long: (21000 - 20000) * 0.1 = 100
	assert.Equal(t, "100", pos.Quote.String())
}

func TestFutures_UnrealizedPnL(t *testing.T) {
	pos := NewFuturesPositionTracker("BTCUSDT",
		fixedpoint.NewFromFloat(10),
		fixedpoint.Zero,
		fixedpoint.Zero,
	)

	// Long position
	pos.Base = fixedpoint.NewFromFloat(0.1)
	pos.AverageCost = fixedpoint.NewFromFloat(20000)

	// Price up: profit
	pnl := pos.UnrealizedPnL(fixedpoint.NewFromFloat(21000))
	assert.Equal(t, "100", pnl.String())

	// Price down: loss
	pnl = pos.UnrealizedPnL(fixedpoint.NewFromFloat(19000))
	assert.Equal(t, "-100", pnl.String())

	// Short position
	pos.Base = fixedpoint.NewFromFloat(-0.1)
	pos.AverageCost = fixedpoint.NewFromFloat(20000)

	// Price down: profit for short
	pnl = pos.UnrealizedPnL(fixedpoint.NewFromFloat(19000))
	assert.Equal(t, "100", pnl.String())

	// Price up: loss for short
	pnl = pos.UnrealizedPnL(fixedpoint.NewFromFloat(21000))
	assert.Equal(t, "-100", pnl.String())
}

func TestFutures_FundingTimeBoundaries(t *testing.T) {
	// Test nextFundingTime
	assert.Equal(t,
		time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC),
		nextFundingTime(time.Date(2025, 1, 1, 3, 0, 0, 0, time.UTC)))

	assert.Equal(t,
		time.Date(2025, 1, 1, 16, 0, 0, 0, time.UTC),
		nextFundingTime(time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)))

	assert.Equal(t,
		time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		nextFundingTime(time.Date(2025, 1, 1, 16, 0, 0, 0, time.UTC)))

	assert.Equal(t,
		time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC),
		nextFundingTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))

	// Test prevFundingTime
	assert.Equal(t,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		prevFundingTime(time.Date(2025, 1, 1, 3, 0, 0, 0, time.UTC)))

	assert.Equal(t,
		time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC),
		prevFundingTime(time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)))

	assert.Equal(t,
		time.Date(2025, 1, 1, 16, 0, 0, 0, time.UTC),
		prevFundingTime(time.Date(2025, 1, 1, 20, 0, 0, 0, time.UTC)))
}
