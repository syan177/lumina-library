package scrapers

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diadata-org/lumina-library/contracts/curve/curvefi"
	"github.com/diadata-org/lumina-library/contracts/curve/curvepool"
	models "github.com/diadata-org/lumina-library/models"
	"github.com/diadata-org/lumina-library/utils"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/event"
)

var (
	restDialCurve   = ""
	wsDialCurve     = ""
	registryAddress = "0x90E00ACe148ca3b23Ac1bC8C240C2a7Dd9c2d7f5"
)

type CurvePair struct {
	OutAsset    models.Asset
	InAsset     models.Asset
	OutIndex    int
	InIndex     int
	ForeignName string
	Address     common.Address
}

type CurveSwap struct {
	ID        string
	Timestamp int64
	Pair      CurvePair
	Amount0   float64
	Amount1   float64
}

type CurveScraper struct {
	exchange         models.Exchange
	poolMap          map[common.Address]CurvePair
	subscribeChannel chan common.Address
	lastTradeTimeMap map[common.Address]time.Time
	restClient       *ethclient.Client
	wsClient         *ethclient.Client
	waitTime         int
}

func NewCurveScraper(ctx context.Context, exchangeName string, blockchain string, pools []models.Pool, tradesChannel chan models.Trade, wg *sync.WaitGroup) {
	var err error
	var scraper CurveScraper
	log.Info("Started Curve scraper.")

	scraper.exchange.Name = exchangeName
	scraper.exchange.Blockchain = blockchain
	scraper.subscribeChannel = make(chan common.Address)
	scraper.lastTradeTimeMap = make(map[common.Address]time.Time)
	scraper.waitTime, err = strconv.Atoi(utils.Getenv(strings.ToUpper(exchangeName)+"_WAIT_TIME", "500"))
	if err != nil {
		log.Error("parse waitTime: ", err)
	}

	scraper.restClient, err = ethclient.Dial(utils.Getenv(CURVE_EXCHANGE+"_URI_REST", restDialCurve))
	if err != nil {
		log.Error("Curve - init rest client: ", err)
	}
	scraper.wsClient, err = ethclient.Dial(utils.Getenv(CURVE_EXCHANGE+"_URI_WS", wsDialCurve))
	if err != nil {
		log.Error("Curve - init ws client: ", err)
	}

	scraper.makePoolMap(pools)

	var lock sync.RWMutex
	go scraper.mainLoop(ctx, pools, tradesChannel, &lock)
}

func (scraper *CurveScraper) mainLoop(ctx context.Context, pools []models.Pool, tradesChannel chan models.Trade, lock *sync.RWMutex) {
	var wg sync.WaitGroup
	for _, pool := range pools {
		lock.Lock()
		scraper.lastTradeTimeMap[common.HexToAddress(pool.Address)] = time.Now()
		lock.Unlock()

		envVar := strings.ToUpper(scraper.exchange.Name) + "_WATCHDOG_" + pool.Address
		watchdogDelay, err := strconv.ParseInt(utils.Getenv(envVar, "60"), 10, 64)
		if err != nil {
			log.Errorf("Curve - Parse curveWatchdogDelay: %v.", err)
		}
		watchdogTicker := time.NewTicker(time.Duration(watchdogDelay) * time.Second)
		go watchdogPool(ctx, scraper.exchange.Name, common.HexToAddress(pool.Address), watchdogTicker, scraper.lastTradeTimeMap, watchdogDelay, scraper.subscribeChannel, lock)

		time.Sleep(time.Duration(scraper.waitTime) * time.Millisecond)
		wg.Add(1)
		go func(ctx context.Context, address common.Address, w *sync.WaitGroup, lock *sync.RWMutex) {
			defer w.Done()
			scraper.watchSwaps(ctx, address, tradesChannel, lock)
		}(ctx, common.HexToAddress(pool.Address), &wg, lock)
	}
	go func() {
		for {
			select {
			case addr := <-scraper.subscribeChannel:
				log.Infof("Resubscribing to pool: %s", addr.Hex())

				lock.Lock()
				scraper.lastTradeTimeMap[addr] = time.Now()
				lock.Unlock()

				go scraper.watchSwaps(ctx, addr, tradesChannel, lock)

			case <-ctx.Done():
				log.Info("Stopping resubscription handler.")
				return
			}
		}
	}()
	wg.Wait()
}

func getSwapDataCurve(swap CurveSwap) (price float64, volume float64) {
	volume = swap.Amount0
	price = swap.Amount0 / swap.Amount1
	return
}

func makeCurveTrade(
	pair CurvePair,
	price float64,
	volume float64,
	timestamp time.Time,
	address common.Address,
	foreignTradeID string,
	exchangeName string,
	blockchain string,
) models.Trade {
	token0 := pair.OutAsset
	token1 := pair.InAsset
	return models.Trade{
		Price:          price,
		Volume:         volume,
		BaseToken:      token1,
		QuoteToken:     token0,
		Time:           timestamp,
		PoolAddress:    address.Hex(),
		ForeignTradeID: foreignTradeID,
		Exchange:       models.Exchange{Name: exchangeName, Blockchain: blockchain},
	}
}

func parseIndexCode(code int) (outIdx, inIdx int, err error) {
	s := strconv.Itoa(code)
	if len(s) != 2 {
		return 0, 0, fmt.Errorf("index code must be 2 digits, got: %s", s)
	}

	outIdx = int(s[0] - '0')
	inIdx = int(s[1] - '0')
	if outIdx < 0 || inIdx < 0 || outIdx == inIdx {
		return 0, 0, fmt.Errorf("invalid index code: %s", s)
	}
	return
}

func (scraper *CurveScraper) watchSwaps(ctx context.Context, address common.Address, tradesChannel chan models.Trade, lock *sync.RWMutex) {
	pair := scraper.poolMap[address]

	sink, subscription, err := scraper.GetSwapsChannel(address)
	if err != nil {
		log.Error("Curve - failed to get swaps channel: ", err)
		return
	} else {
		log.Infof("Curve - subscribed to pool: %s", address.Hex())
	}

	go func() {
		for {
			select {
			case rawSwap, ok := <-sink:
				if ok {
					log.Infof("Curve - received swap: %v", rawSwap)
					swap, err := scraper.normalizeCurveSwap(*rawSwap)
					if err != nil {
						log.Error("Curve - error normalizing swap: ", err)
					}

					price, volume := getSwapDataCurve(swap)
					t := makeCurveTrade(pair, price, volume, time.Unix(swap.Timestamp, 0), address, swap.ID, scraper.exchange.Name, scraper.exchange.Blockchain)

					// Update lastTradeTimeMap
					lock.Lock()
					scraper.lastTradeTimeMap[address] = t.Time
					lock.Unlock()

					tradesChannel <- t

					// switch pair.Order {
					// case 0:
					// 	tradesChannel <- t
					// case 1:
					// 	t.SwapTrade()
					// 	tradesChannel <- t
					// case 2:
					// 	logTrade(t)
					// 	tradesChannel <- t
					// 	t.SwapTrade()
					// 	tradesChannel <- t
					// }
					logTrade(t)
				}
			case err := <-subscription.Err():
				log.Errorf("Subscription error for pool %s: %v", address.Hex(), err)
				scraper.subscribeChannel <- address
				return

			case <-ctx.Done():
				log.Infof("Sutting down watchSwaps for %s", address.Hex())
				return
			}
		}
	}()
}

func (scraper *CurveScraper) normalizeCurveSwap(swap curvepool.CurvepoolTokenExchange) (CurveSwap, error) {
	pair := scraper.poolMap[swap.Raw.Address]
	decimals0 := int(pair.OutAsset.Decimals)
	decimals1 := int(pair.InAsset.Decimals)

	amount0, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.TokensSold), new(big.Float).SetFloat64(math.Pow10(decimals0))).Float64()
	amount1, _ := new(big.Float).Quo(big.NewFloat(0).SetInt(swap.TokensBought), new(big.Float).SetFloat64(math.Pow10(decimals1))).Float64()

	normalizedSwap := CurveSwap{
		ID:        swap.Raw.TxHash.Hex(),
		Timestamp: time.Now().Unix(),
		Pair:      pair,
		Amount0:   amount0,
		Amount1:   amount1,
	}

	return normalizedSwap, nil
}

func (scraper *CurveScraper) GetSwapsChannel(address common.Address) (chan *curvepool.CurvepoolTokenExchange, event.Subscription, error) {
	sink := make(chan *curvepool.CurvepoolTokenExchange)

	pairFiltererContract, err := curvepool.NewCurvepoolFilterer(address, scraper.wsClient)
	if err != nil {
		log.Fatal(err)
	}
	header, err := scraper.restClient.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}
	startblock := header.Number.Uint64() - uint64(20)

	sub, err := pairFiltererContract.WatchTokenExchange(&bind.WatchOpts{Start: &startblock}, sink, nil)

	if err != nil {
		log.Error("error in get swaps channel: ", err)
	}

	return sink, sub, nil
}

func (scraper *CurveScraper) makePoolMap(pools []models.Pool) error {
	scraper.poolMap = make(map[common.Address]CurvePair)
	var assetMap = make(map[common.Address]models.Asset)

	curvefiRegistry, err := curvefi.NewCurvefi(common.HexToAddress(registryAddress), scraper.restClient)
	if err != nil {
		log.Error("Curve - failed to create curvefi instance: ", err)
		return err
	}

	for _, pool := range pools {
		outIdx, inIdx, err := parseIndexCode(pool.Order)
		if err != nil {
			log.Error("Curve - failed to parse index code: ", err)
			return err
		}

		coins, err := curvefiRegistry.GetCoins(&bind.CallOpts{Context: context.Background()}, common.HexToAddress(pool.Address))
		if err != nil {
			log.Error("Curve - failed to get coins: ", err)
			return err
		}

		if coins[outIdx] == (common.Address{}) || coins[inIdx] == (common.Address{}) {
			log.Error("Curve - pool %s - empty coin at index out=%d or in=%d: %v", pool.Address, outIdx, inIdx, err)
			return err
		}

		token0Address := coins[1]
		if _, ok := assetMap[token0Address]; !ok {
			token0, err := models.GetAsset(token0Address, scraper.exchange.Blockchain, scraper.restClient)
			if err != nil {
				return err
			}
			assetMap[token0Address] = token0
		}

		token1Address := coins[2]
		if _, ok := assetMap[token1Address]; !ok {
			token1, err := models.GetAsset(token1Address, scraper.exchange.Blockchain, scraper.restClient)
			if err != nil {
				return err
			}
			assetMap[token1Address] = token1
		}

		scraper.poolMap[common.HexToAddress(pool.Address)] = CurvePair{
			OutAsset: assetMap[token0Address],
			InAsset:  assetMap[token1Address],
			OutIndex: outIdx,
			InIndex:  inIdx,
			Address:  common.HexToAddress(pool.Address),
		}
	}

	return nil
}
