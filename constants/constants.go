package constants

import (
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/chain/types"
)

const DefaultContentSizeLimit = 34_000_000_000
const ContentLocationLocal = "local"
const TopMinerSel = 15
const BucketingEnabled = true
const MinSafeDealLifetime = 2880 * 21 // three weeks

// amount of time a staging zone will remain open before we aggregate it into a piece of content
const MaxStagingZoneLifetime = time.Hour * 8

// maximum amount of time a piece of content will go without either being aggregated or having a deal made for it
const MaxContentAge = time.Hour * 24 * 7

// staging zones will remain open for at least this long after the last piece of content is added to them (unless they are full)
const StagingZoneKeepAlive = time.Minute * 40

const MinDealSize = 256 << 20

const MaxBucketItems = 10000

// Making default deal duration be three weeks less than the maximum to ensure
// miners who start their deals early dont run into issues
const DealDuration = 1555200 - (2880 * 21)

// 90% of the unpadded data size for a 4GB piece
// the 10% gap is to accommodate car file packing overhead, can probably do this better
var IndividualDealThreshold = (abi.PaddedPieceSize(4<<30).Unpadded() * 9) / 10

// 14.29 Gib
var MaxStagingZoneSizeLimit = int64((abi.PaddedPieceSize(16<<30).Unpadded() * 9) / 10)

// 13.29 GiB
var MinStagingZoneSizeLimit = int64(MaxStagingZoneSizeLimit - (1 << 30))

const TokenExpiryDurationAdmin = time.Hour * 24 * 365           // 1 year
const TokenExpiryDurationRegister = time.Hour * 24 * 7          // 1 week
const TokenExpiryDurationLogin = time.Hour * 24 * 30            // 30 days
const TokenExpiryDurationDefault = time.Hour * 24 * 30          // 30 days
const TokenExpiryDurationPermanent = time.Hour * 24 * 365 * 100 // 100 years

var DealMaxPrice abi.TokenAmount
var VerifiedDealMaxPrice = abi.NewTokenAmount(0)

func init() {
	max, err := types.ParseFIL("0.00000003")
	if err != nil {
		panic(err)
	}
	DealMaxPrice = abi.TokenAmount(max)
}
