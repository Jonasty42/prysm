package beacon

import (
	"context"
	"encoding/binary"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gogo/protobuf/proto"
	ptypes "github.com/gogo/protobuf/types"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/go-ssz"
	mock "github.com/prysmaticlabs/prysm/beacon-chain/blockchain/testing"
	"github.com/prysmaticlabs/prysm/beacon-chain/cache"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/db"
	dbTest "github.com/prysmaticlabs/prysm/beacon-chain/db/testing"
	"github.com/prysmaticlabs/prysm/beacon-chain/flags"
	stateTrie "github.com/prysmaticlabs/prysm/beacon-chain/state"
	"github.com/prysmaticlabs/prysm/beacon-chain/state/stategen"
	pbp2p "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/testutil"
)

func init() {
	// Use minimal config to reduce test setup time.
	params.OverrideBeaconConfig(params.MinimalSpecConfig())
	flags.Init(&flags.GlobalFlags{
		MaxPageSize: 250,
	})
}

func TestServer_GetValidatorActiveSetChanges_CannotRequestFutureEpoch(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewBeaconState()
	if err := st.SetSlot(0); err != nil {
		t.Fatal(err)
	}
	bs := &Server{GenesisTimeFetcher: &mock.ChainService{}}

	wanted := "Cannot retrieve information about an epoch in the future"
	if _, err := bs.GetValidatorActiveSetChanges(
		ctx,
		&ethpb.GetValidatorActiveSetChangesRequest{
			QueryFilter: &ethpb.GetValidatorActiveSetChangesRequest_Epoch{
				Epoch: helpers.SlotToEpoch(bs.GenesisTimeFetcher.CurrentSlot()) + 1,
			},
		},
	); err != nil && !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_ListValidatorBalances_CannotRequestFutureEpoch(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	st := testutil.NewBeaconState()
	if err := st.SetSlot(0); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}

	wanted := "Cannot retrieve information about an epoch in the future"
	if _, err := bs.ListValidatorBalances(
		ctx,
		&ethpb.ListValidatorBalancesRequest{
			QueryFilter: &ethpb.ListValidatorBalancesRequest_Epoch{
				Epoch: 1,
			},
		},
	); err != nil && !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_ListValidatorBalances_NoResults(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	st := testutil.NewBeaconState()
	if err := st.SetSlot(0); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}
	wanted := &ethpb.ValidatorBalances{
		Balances:      make([]*ethpb.ValidatorBalances_Balance, 0),
		TotalSize:     int32(0),
		NextPageToken: strconv.Itoa(0),
	}
	res, err := bs.ListValidatorBalances(
		ctx,
		&ethpb.ListValidatorBalancesRequest{
			QueryFilter: &ethpb.ListValidatorBalancesRequest_Epoch{
				Epoch: 0,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(wanted, res) {
		t.Errorf("Wanted %v, received %v", wanted, res)
	}
}

func TestServer_ListValidatorBalances_DefaultResponse_NoArchive(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	numItems := 100
	validators := make([]*ethpb.Validator, numItems)
	balances := make([]uint64, numItems)
	balancesResponse := make([]*ethpb.ValidatorBalances_Balance, numItems)
	for i := 0; i < numItems; i++ {
		validators[i] = &ethpb.Validator{
			PublicKey:             pubKey(uint64(i)),
			WithdrawalCredentials: make([]byte, 32),
		}
		balances[i] = params.BeaconConfig().MaxEffectiveBalance
		balancesResponse[i] = &ethpb.ValidatorBalances_Balance{
			PublicKey: pubKey(uint64(i)),
			Index:     uint64(i),
			Balance:   params.BeaconConfig().MaxEffectiveBalance,
		}
	}
	st := testutil.NewBeaconState()
	if err := st.SetSlot(0); err != nil {
		t.Fatal(err)
	}
	if err := st.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBalances(balances); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}
	res, err := bs.ListValidatorBalances(
		ctx,
		&ethpb.ListValidatorBalancesRequest{
			QueryFilter: &ethpb.ListValidatorBalancesRequest_Epoch{
				Epoch: 0,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(balancesResponse, res.Balances) {
		t.Errorf("Wanted %v, received %v", balancesResponse, res.Balances)
	}
}

func TestServer_ListValidatorBalances_DefaultResponse_FromArchive(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	currentNumValidators := 100
	numOldBalances := 50
	validators := make([]*ethpb.Validator, currentNumValidators)
	balances := make([]uint64, currentNumValidators)
	oldBalances := make([]uint64, numOldBalances)
	balancesResponse := make([]*ethpb.ValidatorBalances_Balance, numOldBalances)
	for i := 0; i < currentNumValidators; i++ {
		key := make([]byte, 48)
		copy(key, strconv.Itoa(i))
		validators[i] = &ethpb.Validator{
			PublicKey:             key,
			WithdrawalCredentials: make([]byte, 32),
		}
		balances[i] = params.BeaconConfig().MaxEffectiveBalance
	}
	for i := 0; i < numOldBalances; i++ {
		oldBalances[i] = params.BeaconConfig().MaxEffectiveBalance
		key := make([]byte, 48)
		copy(key, strconv.Itoa(i))
		balancesResponse[i] = &ethpb.ValidatorBalances_Balance{
			PublicKey: key,
			Index:     uint64(i),
			Balance:   params.BeaconConfig().MaxEffectiveBalance,
		}
	}
	// We archive old balances for epoch 50.
	if err := db.SaveArchivedBalances(ctx, 50, oldBalances); err != nil {
		t.Fatal(err)
	}
	st := testutil.NewBeaconState()
	if err := st.SetSlot(helpers.StartSlot(100) /* epoch 100 */); err != nil {
		t.Fatal(err)
	}
	if err := st.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBalances(balances); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}
	res, err := bs.ListValidatorBalances(
		ctx,
		&ethpb.ListValidatorBalancesRequest{
			QueryFilter: &ethpb.ListValidatorBalancesRequest_Epoch{
				Epoch: 50,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(balancesResponse, res.Balances) {
		t.Errorf("Wanted %v, received %v", balancesResponse, res.Balances)
	}
}

func TestServer_ListValidatorBalances_PaginationOutOfRange(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	setupValidators(t, db, 3)

	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}

	req := &ethpb.ListValidatorBalancesRequest{PageToken: strconv.Itoa(1), PageSize: 100}
	wanted := fmt.Sprintf("page start %d >= list %d", req.PageSize, len(headState.Balances()))
	if _, err := bs.ListValidatorBalances(context.Background(), req); err != nil && !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_ListValidatorBalances_ExceedsMaxPageSize(t *testing.T) {
	bs := &Server{}
	exceedsMax := int32(flags.Get().MaxPageSize + 1)

	wanted := fmt.Sprintf(
		"Requested page size %d can not be greater than max size %d",
		exceedsMax,
		flags.Get().MaxPageSize,
	)
	req := &ethpb.ListValidatorBalancesRequest{PageToken: strconv.Itoa(0), PageSize: exceedsMax}
	if _, err := bs.ListValidatorBalances(context.Background(), req); err != nil && !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func pubKey(i uint64) []byte {
	pubKey := make([]byte, params.BeaconConfig().BLSPubkeyLength)
	binary.LittleEndian.PutUint64(pubKey, i)
	return pubKey
}

func TestServer_ListValidatorBalances_Pagination_Default(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	setupValidators(t, db, 100)

	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		BeaconDB:    db,
		HeadFetcher: &mock.ChainService{State: headState},
	}

	tests := []struct {
		req *ethpb.ListValidatorBalancesRequest
		res *ethpb.ValidatorBalances
	}{
		{req: &ethpb.ListValidatorBalancesRequest{PublicKeys: [][]byte{pubKey(99)}},
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{Index: 99, PublicKey: pubKey(99), Balance: 99},
				},
				NextPageToken: "",
				TotalSize:     1,
			},
		},
		{req: &ethpb.ListValidatorBalancesRequest{Indices: []uint64{1, 2, 3}},
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{Index: 1, PublicKey: pubKey(1), Balance: 1},
					{Index: 2, PublicKey: pubKey(2), Balance: 2},
					{Index: 3, PublicKey: pubKey(3), Balance: 3},
				},
				NextPageToken: "",
				TotalSize:     3,
			},
		},
		{req: &ethpb.ListValidatorBalancesRequest{PublicKeys: [][]byte{pubKey(10), pubKey(11), pubKey(12)}},
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{Index: 10, PublicKey: pubKey(10), Balance: 10},
					{Index: 11, PublicKey: pubKey(11), Balance: 11},
					{Index: 12, PublicKey: pubKey(12), Balance: 12},
				},
				NextPageToken: "",
				TotalSize:     3,
			}},
		{req: &ethpb.ListValidatorBalancesRequest{PublicKeys: [][]byte{pubKey(2), pubKey(3)}, Indices: []uint64{3, 4}}, // Duplication
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{Index: 2, PublicKey: pubKey(2), Balance: 2},
					{Index: 3, PublicKey: pubKey(3), Balance: 3},
					{Index: 4, PublicKey: pubKey(4), Balance: 4},
				},
				NextPageToken: "",
				TotalSize:     3,
			}},
		{req: &ethpb.ListValidatorBalancesRequest{PublicKeys: [][]byte{{}}, Indices: []uint64{3, 4}}, // Public key has a blank value
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{Index: 3, PublicKey: pubKey(3), Balance: 3},
					{Index: 4, PublicKey: pubKey(4), Balance: 4},
				},
				NextPageToken: "",
				TotalSize:     2,
			}},
	}
	for _, test := range tests {
		res, err := bs.ListValidatorBalances(context.Background(), test.req)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(res, test.res) {
			t.Errorf("Expected %v, received %v", test.res, res)
		}
	}
}

func TestServer_ListValidatorBalances_Pagination_CustomPageSizes(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	count := 1000
	setupValidators(t, db, count)

	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}

	tests := []struct {
		req *ethpb.ListValidatorBalancesRequest
		res *ethpb.ValidatorBalances
	}{
		{req: &ethpb.ListValidatorBalancesRequest{PageToken: strconv.Itoa(1), PageSize: 3},
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{PublicKey: pubKey(3), Index: 3, Balance: uint64(3)},
					{PublicKey: pubKey(4), Index: 4, Balance: uint64(4)},
					{PublicKey: pubKey(5), Index: 5, Balance: uint64(5)}},
				NextPageToken: strconv.Itoa(2),
				TotalSize:     int32(count)}},
		{req: &ethpb.ListValidatorBalancesRequest{PageToken: strconv.Itoa(10), PageSize: 5},
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{PublicKey: pubKey(50), Index: 50, Balance: uint64(50)},
					{PublicKey: pubKey(51), Index: 51, Balance: uint64(51)},
					{PublicKey: pubKey(52), Index: 52, Balance: uint64(52)},
					{PublicKey: pubKey(53), Index: 53, Balance: uint64(53)},
					{PublicKey: pubKey(54), Index: 54, Balance: uint64(54)}},
				NextPageToken: strconv.Itoa(11),
				TotalSize:     int32(count)}},
		{req: &ethpb.ListValidatorBalancesRequest{PageToken: strconv.Itoa(33), PageSize: 3},
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{PublicKey: pubKey(99), Index: 99, Balance: uint64(99)},
					{PublicKey: pubKey(100), Index: 100, Balance: uint64(100)},
					{PublicKey: pubKey(101), Index: 101, Balance: uint64(101)},
				},
				NextPageToken: "34",
				TotalSize:     int32(count)}},
		{req: &ethpb.ListValidatorBalancesRequest{PageSize: 2},
			res: &ethpb.ValidatorBalances{
				Balances: []*ethpb.ValidatorBalances_Balance{
					{PublicKey: pubKey(0), Index: 0, Balance: uint64(0)},
					{PublicKey: pubKey(1), Index: 1, Balance: uint64(1)}},
				NextPageToken: strconv.Itoa(1),
				TotalSize:     int32(count)}},
	}
	for _, test := range tests {
		res, err := bs.ListValidatorBalances(context.Background(), test.req)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(res, test.res) {
			t.Errorf("Expected %v, received %v", test.res, res)
		}
	}
}

func TestServer_ListValidatorBalances_OutOfRange(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)
	setupValidators(t, db, 1)

	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		BeaconDB:    db,
		HeadFetcher: &mock.ChainService{State: headState},
	}

	req := &ethpb.ListValidatorBalancesRequest{Indices: []uint64{uint64(1)}}
	wanted := "does not exist"
	if _, err := bs.ListValidatorBalances(context.Background(), req); err == nil || !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_ListValidatorBalances_FromArchive(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)
	ctx := context.Background()
	epoch := uint64(0)
	validators, balances := setupValidators(t, db, 100)

	if err := db.SaveArchivedBalances(ctx, epoch, balances); err != nil {
		t.Fatal(err)
	}

	newerBalances := make([]uint64, len(balances))
	for i := 0; i < len(newerBalances); i++ {
		newerBalances[i] = balances[i] * 2
	}
	st := testutil.NewBeaconState()
	if err := st.SetSlot(params.BeaconConfig().SlotsPerEpoch * 3); err != nil {
		t.Fatal(err)
	}
	if err := st.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBalances(newerBalances); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}

	req := &ethpb.ListValidatorBalancesRequest{
		QueryFilter: &ethpb.ListValidatorBalancesRequest_Epoch{Epoch: 0},
		Indices:     []uint64{uint64(1)},
	}
	res, err := bs.ListValidatorBalances(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// We should expect a response containing the old balance from epoch 0,
	// not the new balance from the current state.
	want := []*ethpb.ValidatorBalances_Balance{
		{
			PublicKey: validators[1].PublicKey,
			Index:     1,
			Balance:   balances[1],
		},
	}
	if !reflect.DeepEqual(want, res.Balances) {
		t.Errorf("Wanted %v, received %v", want, res.Balances)
	}
}

func TestServer_ListValidatorBalances_FromArchive_NewValidatorNotFound(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)
	ctx := context.Background()
	epoch := uint64(0)
	_, balances := setupValidators(t, db, 100)

	if err := db.SaveArchivedBalances(ctx, epoch, balances); err != nil {
		t.Fatal(err)
	}

	newValidators, newBalances := setupValidators(t, db, 200)
	st := testutil.NewBeaconState()
	if err := st.SetSlot(params.BeaconConfig().SlotsPerEpoch * 3); err != nil {
		t.Fatal(err)
	}
	if err := st.SetValidators(newValidators); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBalances(newBalances); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}

	req := &ethpb.ListValidatorBalancesRequest{
		QueryFilter: &ethpb.ListValidatorBalancesRequest_Epoch{Epoch: 0},
		Indices:     []uint64{1, 150, 161},
	}
	if _, err := bs.ListValidatorBalances(context.Background(), req); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Wanted out of range error for including newer validators in the arguments, received %v", err)
	}
}

func TestServer_ListValidators_CannotRequestFutureEpoch(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	st := testutil.NewBeaconState()
	if err := st.SetSlot(0); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}

	wanted := "Cannot retrieve information about an epoch in the future"
	if _, err := bs.ListValidators(
		ctx,
		&ethpb.ListValidatorsRequest{
			QueryFilter: &ethpb.ListValidatorsRequest_Epoch{
				Epoch: 1,
			},
		},
	); err != nil && !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_ListValidators_NoResults(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	st := testutil.NewBeaconState()
	if err := st.SetSlot(0); err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}
	wanted := &ethpb.Validators{
		ValidatorList: make([]*ethpb.Validators_ValidatorContainer, 0),
		TotalSize:     int32(0),
		NextPageToken: strconv.Itoa(0),
	}
	res, err := bs.ListValidators(
		ctx,
		&ethpb.ListValidatorsRequest{
			QueryFilter: &ethpb.ListValidatorsRequest_Epoch{
				Epoch: 0,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(wanted, res) {
		t.Errorf("Wanted %v, received %v", wanted, res)
	}
}

func TestServer_ListValidators_OnlyActiveValidators(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	count := 100
	balances := make([]uint64, count)
	validators := make([]*ethpb.Validator, count)
	activeValidators := make([]*ethpb.Validators_ValidatorContainer, 0)
	for i := 0; i < count; i++ {
		pubKey := pubKey(uint64(i))
		balances[i] = params.BeaconConfig().MaxEffectiveBalance

		// We mark even validators as active, and odd validators as inactive.
		if i%2 == 0 {
			val := &ethpb.Validator{
				PublicKey:             pubKey,
				WithdrawalCredentials: make([]byte, 32),
				ActivationEpoch:       0,
				ExitEpoch:             params.BeaconConfig().FarFutureEpoch,
			}
			validators[i] = val
			activeValidators = append(activeValidators, &ethpb.Validators_ValidatorContainer{
				Index:     uint64(i),
				Validator: val,
			})
		} else {
			validators[i] = &ethpb.Validator{
				PublicKey:             pubKey,
				WithdrawalCredentials: make([]byte, 32),
				ActivationEpoch:       0,
				ExitEpoch:             0,
			}
		}
	}
	st := testutil.NewBeaconState()
	if err := st.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBalances(balances); err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}

	received, err := bs.ListValidators(ctx, &ethpb.ListValidatorsRequest{
		Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(activeValidators, received.ValidatorList) {
		t.Errorf("Wanted %v, received %v", activeValidators, received.ValidatorList)
	}
}

func TestServer_ListValidators_NoPagination(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	validators, _ := setupValidators(t, db, 100)
	want := make([]*ethpb.Validators_ValidatorContainer, len(validators))
	for i := 0; i < len(validators); i++ {
		want[i] = &ethpb.Validators_ValidatorContainer{
			Index:     uint64(i),
			Validator: validators[i],
		}
	}
	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
		FinalizationFetcher: &mock.ChainService{
			FinalizedCheckPoint: &ethpb.Checkpoint{
				Epoch: 0,
			},
		},
	}

	received, err := bs.ListValidators(context.Background(), &ethpb.ListValidatorsRequest{})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(want, received.ValidatorList) {
		t.Fatal("Incorrect respond of validators")
	}
}

func TestServer_ListValidators_IndicesPubKeys(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	validators, _ := setupValidators(t, db, 100)
	indicesWanted := []uint64{2, 7, 11, 17}
	pubkeyIndicesWanted := []uint64{3, 5, 9, 15}
	allIndicesWanted := append(indicesWanted, pubkeyIndicesWanted...)
	want := make([]*ethpb.Validators_ValidatorContainer, len(allIndicesWanted))
	for i, idx := range allIndicesWanted {
		want[i] = &ethpb.Validators_ValidatorContainer{
			Index:     idx,
			Validator: validators[idx],
		}
	}
	sort.Slice(want, func(i int, j int) bool {
		return want[i].Index < want[j].Index
	})

	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
		FinalizationFetcher: &mock.ChainService{
			FinalizedCheckPoint: &ethpb.Checkpoint{
				Epoch: 0,
			},
		},
	}

	pubKeysWanted := make([][]byte, len(pubkeyIndicesWanted))
	for i, indice := range pubkeyIndicesWanted {
		pubKeysWanted[i] = pubKey(indice)
	}
	req := &ethpb.ListValidatorsRequest{
		Indices:    indicesWanted,
		PublicKeys: pubKeysWanted,
	}
	received, err := bs.ListValidators(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(want, received.ValidatorList) {
		t.Fatal("Incorrect respond of validators")
	}
}

func TestServer_ListValidators_Pagination(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	count := 100
	setupValidators(t, db, count)

	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
		FinalizationFetcher: &mock.ChainService{
			FinalizedCheckPoint: &ethpb.Checkpoint{
				Epoch: 0,
			},
		},
	}

	tests := []struct {
		req *ethpb.ListValidatorsRequest
		res *ethpb.Validators
	}{
		{req: &ethpb.ListValidatorsRequest{PageToken: strconv.Itoa(1), PageSize: 3},
			res: &ethpb.Validators{
				ValidatorList: []*ethpb.Validators_ValidatorContainer{
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(3),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 3,
					},
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(4),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 4,
					},
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(5),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 5,
					},
				},
				NextPageToken: strconv.Itoa(2),
				TotalSize:     int32(count)}},
		{req: &ethpb.ListValidatorsRequest{PageToken: strconv.Itoa(10), PageSize: 5},
			res: &ethpb.Validators{
				ValidatorList: []*ethpb.Validators_ValidatorContainer{
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(50),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 50,
					},
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(51),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 51,
					},
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(52),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 52,
					},
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(53),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 53,
					},
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(54),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 54,
					},
				},
				NextPageToken: strconv.Itoa(11),
				TotalSize:     int32(count)}},
		{req: &ethpb.ListValidatorsRequest{PageToken: strconv.Itoa(33), PageSize: 3},
			res: &ethpb.Validators{
				ValidatorList: []*ethpb.Validators_ValidatorContainer{
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(99),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 99,
					},
				},
				NextPageToken: "",
				TotalSize:     int32(count)}},
		{req: &ethpb.ListValidatorsRequest{PageSize: 2},
			res: &ethpb.Validators{
				ValidatorList: []*ethpb.Validators_ValidatorContainer{
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(0),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 0,
					},
					{
						Validator: &ethpb.Validator{
							PublicKey:             pubKey(1),
							WithdrawalCredentials: make([]byte, 32),
						},
						Index: 1,
					},
				},
				NextPageToken: strconv.Itoa(1),
				TotalSize:     int32(count)}},
	}
	for _, test := range tests {
		res, err := bs.ListValidators(context.Background(), test.req)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(res, test.res) {
			t.Errorf("Incorrect validator response, wanted %v, received %v", test.res, res)
		}
	}
}

func TestServer_ListValidators_PaginationOutOfRange(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	count := 1
	validators, _ := setupValidators(t, db, count)
	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
		FinalizationFetcher: &mock.ChainService{
			FinalizedCheckPoint: &ethpb.Checkpoint{
				Epoch: 0,
			},
		},
	}

	req := &ethpb.ListValidatorsRequest{PageToken: strconv.Itoa(1), PageSize: 100}
	wanted := fmt.Sprintf("page start %d >= list %d", req.PageSize, len(validators))
	if _, err := bs.ListValidators(context.Background(), req); err == nil || !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_ListValidators_ExceedsMaxPageSize(t *testing.T) {
	bs := &Server{}
	exceedsMax := int32(flags.Get().MaxPageSize + 1)

	wanted := fmt.Sprintf("Requested page size %d can not be greater than max size %d", exceedsMax, flags.Get().MaxPageSize)
	req := &ethpb.ListValidatorsRequest{PageToken: strconv.Itoa(0), PageSize: exceedsMax}
	if _, err := bs.ListValidators(context.Background(), req); err == nil || !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_ListValidators_DefaultPageSize(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	validators, _ := setupValidators(t, db, 1000)
	want := make([]*ethpb.Validators_ValidatorContainer, len(validators))
	for i := 0; i < len(validators); i++ {
		want[i] = &ethpb.Validators_ValidatorContainer{
			Index:     uint64(i),
			Validator: validators[i],
		}
	}
	headState, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
		FinalizationFetcher: &mock.ChainService{
			FinalizedCheckPoint: &ethpb.Checkpoint{
				Epoch: 0,
			},
		},
	}

	req := &ethpb.ListValidatorsRequest{}
	res, err := bs.ListValidators(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	i := 0
	j := params.BeaconConfig().DefaultPageSize
	if !reflect.DeepEqual(res.ValidatorList, want[i:j]) {
		t.Error("Incorrect respond of validators")
	}
}

func TestServer_ListValidators_FromOldEpoch(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	numEpochs := 30
	validators := make([]*ethpb.Validator, numEpochs)
	for i := 0; i < numEpochs; i++ {
		validators[i] = &ethpb.Validator{
			ActivationEpoch:       uint64(i),
			PublicKey:             make([]byte, 48),
			WithdrawalCredentials: make([]byte, 32),
		}
	}
	want := make([]*ethpb.Validators_ValidatorContainer, len(validators))
	for i := 0; i < len(validators); i++ {
		want[i] = &ethpb.Validators_ValidatorContainer{
			Index:     uint64(i),
			Validator: validators[i],
		}
	}

	st := testutil.NewBeaconState()
	if err := st.SetSlot(helpers.StartSlot(30)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}

	req := &ethpb.ListValidatorsRequest{
		QueryFilter: &ethpb.ListValidatorsRequest_Genesis{
			Genesis: true,
		},
	}
	res, err := bs.ListValidators(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ValidatorList) != 1 {
		t.Errorf("Wanted 1 validator at genesis, received %d", len(res.ValidatorList))
	}

	req = &ethpb.ListValidatorsRequest{
		QueryFilter: &ethpb.ListValidatorsRequest_Epoch{
			Epoch: 20,
		},
	}
	res, err = bs.ListValidators(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.ValidatorList, want[:21]) {
		t.Errorf("Incorrect number of validators, wanted %d received %d", len(want[:21]), len(res.ValidatorList))
	}
}

func TestServer_GetValidator(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	count := 30
	validators := make([]*ethpb.Validator, count)
	for i := 0; i < count; i++ {
		validators[i] = &ethpb.Validator{
			ActivationEpoch:       uint64(i),
			PublicKey:             pubKey(uint64(i)),
			WithdrawalCredentials: make([]byte, 32),
		}
	}

	st := testutil.NewBeaconState()
	if err := st.SetValidators(validators); err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: st,
		},
	}

	tests := []struct {
		req     *ethpb.GetValidatorRequest
		res     *ethpb.Validator
		wantErr bool
		err     string
	}{
		{
			req: &ethpb.GetValidatorRequest{
				QueryFilter: &ethpb.GetValidatorRequest_Index{
					Index: 0,
				},
			},
			res:     validators[0],
			wantErr: false,
		},
		{
			req: &ethpb.GetValidatorRequest{
				QueryFilter: &ethpb.GetValidatorRequest_Index{
					Index: uint64(count - 1),
				},
			},
			res:     validators[count-1],
			wantErr: false,
		},
		{
			req: &ethpb.GetValidatorRequest{
				QueryFilter: &ethpb.GetValidatorRequest_PublicKey{
					PublicKey: pubKey(5),
				},
			},
			res:     validators[5],
			wantErr: false,
		},
		{
			req: &ethpb.GetValidatorRequest{
				QueryFilter: &ethpb.GetValidatorRequest_PublicKey{
					PublicKey: []byte("bad-keyxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"),
				},
			},
			res:     nil,
			wantErr: true,
			err:     "No validator matched filter criteria",
		},
		{
			req: &ethpb.GetValidatorRequest{
				QueryFilter: &ethpb.GetValidatorRequest_Index{
					Index: uint64(len(validators)),
				},
			},
			res:     nil,
			wantErr: true,
			err:     fmt.Sprintf("there are only %d validators", len(validators)),
		},
	}

	for _, test := range tests {
		res, err := bs.GetValidator(context.Background(), test.req)
		if test.wantErr && err != nil {
			if !strings.Contains(err.Error(), test.err) {
				t.Fatalf("Wanted %v, received %v", test.err, err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(test.res, res) {
			t.Errorf("Wanted %v, got %v", test.res, res)
		}
	}
}

func TestServer_GetValidatorActiveSetChanges(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	validators := make([]*ethpb.Validator, 8)
	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(0); err != nil {
		t.Fatal(err)
	}
	if err := headState.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(validators); i++ {
		activationEpoch := params.BeaconConfig().FarFutureEpoch
		withdrawableEpoch := params.BeaconConfig().FarFutureEpoch
		exitEpoch := params.BeaconConfig().FarFutureEpoch
		slashed := false
		balance := params.BeaconConfig().MaxEffectiveBalance
		// Mark indices divisible by two as activated.
		if i%2 == 0 {
			activationEpoch = 0
		} else if i%3 == 0 {
			// Mark indices divisible by 3 as slashed.
			withdrawableEpoch = params.BeaconConfig().EpochsPerSlashingsVector
			slashed = true
		} else if i%5 == 0 {
			// Mark indices divisible by 5 as exited.
			exitEpoch = 0
			withdrawableEpoch = params.BeaconConfig().MinValidatorWithdrawabilityDelay
		} else if i%7 == 0 {
			// Mark indices divisible by 7 as ejected.
			exitEpoch = 0
			withdrawableEpoch = params.BeaconConfig().MinValidatorWithdrawabilityDelay
			balance = params.BeaconConfig().EjectionBalance
		}
		if err := headState.UpdateValidatorAtIndex(uint64(i), &ethpb.Validator{
			ActivationEpoch:       activationEpoch,
			PublicKey:             pubKey(uint64(i)),
			EffectiveBalance:      balance,
			WithdrawalCredentials: make([]byte, 32),
			WithdrawableEpoch:     withdrawableEpoch,
			Slashed:               slashed,
			ExitEpoch:             exitEpoch,
		}); err != nil {
			t.Fatal(err)
		}
	}
	b := &ethpb.SignedBeaconBlock{Block: &ethpb.BeaconBlock{}}
	if err := db.SaveBlock(ctx, b); err != nil {
		t.Fatal(err)
	}

	gRoot, err := ssz.HashTreeRoot(b.Block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveGenesisBlockRoot(ctx, gRoot); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveState(ctx, headState, gRoot); err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		FinalizationFetcher: &mock.ChainService{
			FinalizedCheckPoint: &ethpb.Checkpoint{Epoch: 0},
		},
		GenesisTimeFetcher: &mock.ChainService{},
		StateGen:           stategen.New(db, cache.NewStateSummaryCache()),
	}
	res, err := bs.GetValidatorActiveSetChanges(ctx, &ethpb.GetValidatorActiveSetChangesRequest{
		QueryFilter: &ethpb.GetValidatorActiveSetChangesRequest_Genesis{Genesis: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantedActive := [][]byte{
		pubKey(0),
		pubKey(2),
		pubKey(4),
		pubKey(6),
	}
	wantedActiveIndices := []uint64{0, 2, 4, 6}
	wantedExited := [][]byte{
		pubKey(5),
	}
	wantedExitedIndices := []uint64{5}
	wantedSlashed := [][]byte{
		pubKey(3),
	}
	wantedSlashedIndices := []uint64{3}
	wantedEjected := [][]byte{
		pubKey(7),
	}
	wantedEjectedIndices := []uint64{7}
	wanted := &ethpb.ActiveSetChanges{
		Epoch:               0,
		ActivatedPublicKeys: wantedActive,
		ActivatedIndices:    wantedActiveIndices,
		ExitedPublicKeys:    wantedExited,
		ExitedIndices:       wantedExitedIndices,
		SlashedPublicKeys:   wantedSlashed,
		SlashedIndices:      wantedSlashedIndices,
		EjectedPublicKeys:   wantedEjected,
		EjectedIndices:      wantedEjectedIndices,
	}
	if !proto.Equal(wanted, res) {
		t.Errorf("Wanted \n%v, received \n%v", wanted, res)
	}
}

func TestServer_GetValidatorActiveSetChanges_FromArchive(t *testing.T) {
	config := &featureconfig.Flags{
		NewStateMgmt: false,
	}
	featureconfig.Init(config)

	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)
	ctx := context.Background()
	validators := make([]*ethpb.Validator, 8)
	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(helpers.StartSlot(100)); err != nil {
		t.Fatal(err)
	}
	if err := headState.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	activatedIndices := make([]uint64, 0)
	exitedIndices := make([]uint64, 0)
	slashedIndices := make([]uint64, 0)
	ejectedIndices := make([]uint64, 0)
	for i := 0; i < len(validators); i++ {
		// Mark indices divisible by two as activated.
		if i%2 == 0 {
			activatedIndices = append(activatedIndices, uint64(i))
		} else if i%3 == 0 {
			// Mark indices divisible by 3 as slashed.
			slashedIndices = append(slashedIndices, uint64(i))
		} else if i%5 == 0 {
			// Mark indices divisible by 5 as exited.
			exitedIndices = append(exitedIndices, uint64(i))
		} else if i%7 == 0 {
			// Mark indices divisible by 7 as ejected.
			ejectedIndices = append(ejectedIndices, uint64(i))
		}
		key := make([]byte, 48)
		copy(key, strconv.Itoa(i))
		if err := headState.UpdateValidatorAtIndex(uint64(i), &ethpb.Validator{
			PublicKey: key,
		}); err != nil {
			t.Fatal(err)
		}
	}
	archivedChanges := &pbp2p.ArchivedActiveSetChanges{
		Activated: activatedIndices,
		Exited:    exitedIndices,
		Slashed:   slashedIndices,
		Ejected:   ejectedIndices,
	}
	// We store the changes during the genesis epoch.
	if err := db.SaveArchivedActiveValidatorChanges(ctx, 0, archivedChanges); err != nil {
		t.Fatal(err)
	}
	// We store the same changes during epoch 5 for further testing.
	if err := db.SaveArchivedActiveValidatorChanges(ctx, 5, archivedChanges); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}
	res, err := bs.GetValidatorActiveSetChanges(ctx, &ethpb.GetValidatorActiveSetChangesRequest{
		QueryFilter: &ethpb.GetValidatorActiveSetChangesRequest_Genesis{Genesis: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantedKeys := make([][]byte, 8)
	for i := 0; i < len(wantedKeys); i++ {
		k := make([]byte, 48)
		copy(k, strconv.Itoa(i))
		wantedKeys[i] = k
	}
	wantedActive := [][]byte{
		wantedKeys[0],
		wantedKeys[2],
		wantedKeys[4],
		wantedKeys[6],
	}
	wantedActiveIndices := []uint64{0, 2, 4, 6}
	wantedExited := [][]byte{
		wantedKeys[5],
	}
	wantedExitedIndices := []uint64{5}
	wantedSlashed := [][]byte{
		wantedKeys[3],
	}
	wantedSlashedIndices := []uint64{3}
	wantedEjected := [][]byte{
		wantedKeys[7],
	}
	wantedEjectedIndices := []uint64{7}
	wanted := &ethpb.ActiveSetChanges{
		Epoch:               0,
		ActivatedPublicKeys: wantedActive,
		ActivatedIndices:    wantedActiveIndices,
		ExitedPublicKeys:    wantedExited,
		ExitedIndices:       wantedExitedIndices,
		SlashedPublicKeys:   wantedSlashed,
		SlashedIndices:      wantedSlashedIndices,
		EjectedPublicKeys:   wantedEjected,
		EjectedIndices:      wantedEjectedIndices,
	}
	if !proto.Equal(wanted, res) {
		t.Errorf("Wanted \n%v, received \n%v", wanted, res)
	}
	res, err = bs.GetValidatorActiveSetChanges(ctx, &ethpb.GetValidatorActiveSetChangesRequest{
		QueryFilter: &ethpb.GetValidatorActiveSetChangesRequest_Epoch{Epoch: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	wanted.Epoch = 5
	if !proto.Equal(wanted, res) {
		t.Errorf("Wanted \n%v, received \n%v", wanted, res)
	}
}

func TestServer_GetValidatorQueue_PendingActivation(t *testing.T) {
	headState, err := stateTrie.InitializeFromProto(&pbp2p.BeaconState{
		Validators: []*ethpb.Validator{
			{
				ActivationEpoch:            helpers.ActivationExitEpoch(0),
				ActivationEligibilityEpoch: 3,
				PublicKey:                  pubKey(3),
				WithdrawalCredentials:      make([]byte, 32),
			},
			{
				ActivationEpoch:            helpers.ActivationExitEpoch(0),
				ActivationEligibilityEpoch: 2,
				PublicKey:                  pubKey(2),
				WithdrawalCredentials:      make([]byte, 32),
			},
			{
				ActivationEpoch:            helpers.ActivationExitEpoch(0),
				ActivationEligibilityEpoch: 1,
				PublicKey:                  pubKey(1),
				WithdrawalCredentials:      make([]byte, 32),
			},
		},
		FinalizedCheckpoint: &ethpb.Checkpoint{
			Epoch: 0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}
	res, err := bs.GetValidatorQueue(context.Background(), &ptypes.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	// We verify the keys are properly sorted by the validators' activation eligibility epoch.
	wanted := [][]byte{
		pubKey(1),
		pubKey(2),
		pubKey(3),
	}
	activeValidatorCount, err := helpers.ActiveValidatorCount(headState, helpers.CurrentEpoch(headState))
	if err != nil {
		t.Fatal(err)
	}
	wantChurn, err := helpers.ValidatorChurnLimit(activeValidatorCount)
	if err != nil {
		t.Fatal(err)
	}
	if res.ChurnLimit != wantChurn {
		t.Errorf("Wanted churn %d, received %d", wantChurn, res.ChurnLimit)
	}
	if !reflect.DeepEqual(res.ActivationPublicKeys, wanted) {
		t.Errorf("Wanted %v, received %v", wanted, res.ActivationPublicKeys)
	}
}

func TestServer_GetValidatorQueue_ExitedValidatorLeavesQueue(t *testing.T) {
	validators := []*ethpb.Validator{
		{
			ActivationEpoch:   0,
			ExitEpoch:         params.BeaconConfig().FarFutureEpoch,
			WithdrawableEpoch: params.BeaconConfig().FarFutureEpoch,
			PublicKey:         []byte("1"),
		},
		{
			ActivationEpoch:   0,
			ExitEpoch:         4,
			WithdrawableEpoch: 6,
			PublicKey:         []byte("2"),
		},
	}

	headState := testutil.NewBeaconState()
	if err := headState.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	if err := headState.SetFinalizedCheckpoint(&ethpb.Checkpoint{Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}

	// First we check if validator with index 1 is in the exit queue.
	res, err := bs.GetValidatorQueue(context.Background(), &ptypes.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	wanted := [][]byte{
		[]byte("2"),
	}
	activeValidatorCount, err := helpers.ActiveValidatorCount(headState, helpers.CurrentEpoch(headState))
	if err != nil {
		t.Fatal(err)
	}
	wantChurn, err := helpers.ValidatorChurnLimit(activeValidatorCount)
	if err != nil {
		t.Fatal(err)
	}
	if res.ChurnLimit != wantChurn {
		t.Errorf("Wanted churn %d, received %d", wantChurn, res.ChurnLimit)
	}
	if !reflect.DeepEqual(res.ExitPublicKeys, wanted) {
		t.Errorf("Wanted %v, received %v", wanted, res.ExitPublicKeys)
	}

	// Now, we move the state.slot past the exit epoch of the validator, and now
	// the validator should no longer exist in the queue.
	if err := headState.SetSlot(helpers.StartSlot(validators[1].ExitEpoch + 1)); err != nil {
		t.Fatal(err)
	}
	res, err = bs.GetValidatorQueue(context.Background(), &ptypes.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExitPublicKeys) != 0 {
		t.Errorf("Wanted empty exit queue, received %v", res.ExitPublicKeys)
	}
}

func TestServer_GetValidatorQueue_PendingExit(t *testing.T) {
	headState, err := stateTrie.InitializeFromProto(&pbp2p.BeaconState{
		Validators: []*ethpb.Validator{
			{
				ActivationEpoch:       0,
				ExitEpoch:             4,
				WithdrawableEpoch:     3,
				PublicKey:             pubKey(3),
				WithdrawalCredentials: make([]byte, 32),
			},
			{
				ActivationEpoch:       0,
				ExitEpoch:             4,
				WithdrawableEpoch:     2,
				PublicKey:             pubKey(2),
				WithdrawalCredentials: make([]byte, 32),
			},
			{
				ActivationEpoch:       0,
				ExitEpoch:             4,
				WithdrawableEpoch:     1,
				PublicKey:             pubKey(1),
				WithdrawalCredentials: make([]byte, 32),
			},
		},
		FinalizedCheckpoint: &ethpb.Checkpoint{
			Epoch: 0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}
	res, err := bs.GetValidatorQueue(context.Background(), &ptypes.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	// We verify the keys are properly sorted by the validators' withdrawable epoch.
	wanted := [][]byte{
		pubKey(1),
		pubKey(2),
		pubKey(3),
	}
	activeValidatorCount, err := helpers.ActiveValidatorCount(headState, helpers.CurrentEpoch(headState))
	if err != nil {
		t.Fatal(err)
	}
	wantChurn, err := helpers.ValidatorChurnLimit(activeValidatorCount)
	if err != nil {
		t.Fatal(err)
	}
	if res.ChurnLimit != wantChurn {
		t.Errorf("Wanted churn %d, received %d", wantChurn, res.ChurnLimit)
	}
	if !reflect.DeepEqual(res.ExitPublicKeys, wanted) {
		t.Errorf("Wanted %v, received %v", wanted, res.ExitPublicKeys)
	}
}

func TestServer_GetValidatorParticipation_CannotRequestCurrentEpoch(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(helpers.StartSlot(2)); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}

	wanted := "Cannot retrieve information about an epoch currently in progress"
	if _, err := bs.GetValidatorParticipation(
		ctx,
		&ethpb.GetValidatorParticipationRequest{
			QueryFilter: &ethpb.GetValidatorParticipationRequest_Epoch{
				Epoch: 2,
			},
		},
	); err != nil && !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_GetValidatorParticipation_CannotRequestFutureEpoch(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(0); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
		GenesisTimeFetcher: &mock.ChainService{},
	}

	wanted := "Cannot retrieve information about an epoch in the future"
	if _, err := bs.GetValidatorParticipation(
		ctx,
		&ethpb.GetValidatorParticipationRequest{
			QueryFilter: &ethpb.GetValidatorParticipationRequest_Epoch{
				Epoch: helpers.SlotToEpoch(bs.GenesisTimeFetcher.CurrentSlot()) + 1,
			},
		},
	); err != nil && !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected error %v, received %v", wanted, err)
	}
}

func TestServer_GetValidatorParticipation_FromArchive(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)
	ctx := context.Background()
	epoch := uint64(4)
	part := &ethpb.ValidatorParticipation{
		GlobalParticipationRate: 1.0,
		VotedEther:              20,
		EligibleEther:           20,
	}
	if err := db.SaveArchivedValidatorParticipation(ctx, epoch-2, part); err != nil {
		t.Fatal(err)
	}

	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(helpers.StartSlot(epoch + 1)); err != nil {
		t.Fatal(err)
	}
	if err := headState.SetFinalizedCheckpoint(&ethpb.Checkpoint{Epoch: epoch + 1}); err != nil {
		t.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}
	if _, err := bs.GetValidatorParticipation(ctx, &ethpb.GetValidatorParticipationRequest{
		QueryFilter: &ethpb.GetValidatorParticipationRequest_Epoch{
			Epoch: epoch + 2,
		},
	}); err == nil {
		t.Error("Expected error when requesting future epoch, received nil")
	}
	// We request data from epoch 0, which we didn't archive, so we should expect an error.
	if _, err := bs.GetValidatorParticipation(ctx, &ethpb.GetValidatorParticipationRequest{
		QueryFilter: &ethpb.GetValidatorParticipationRequest_Genesis{
			Genesis: true,
		},
	}); err == nil {
		t.Error("Expected error when data from archive is not found, received nil")
	}

	want := &ethpb.ValidatorParticipationResponse{
		Epoch:         epoch - 2,
		Finalized:     true,
		Participation: part,
	}
	res, err := bs.GetValidatorParticipation(ctx, &ethpb.GetValidatorParticipationRequest{
		QueryFilter: &ethpb.GetValidatorParticipationRequest_Epoch{
			Epoch: epoch - 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(want, res) {
		t.Errorf("Wanted %v, received %v", want, res)
	}
}

func TestServer_GetValidatorParticipation_PrevEpoch(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)

	ctx := context.Background()
	validatorCount := uint64(100)

	validators := make([]*ethpb.Validator, validatorCount)
	balances := make([]uint64, validatorCount)
	for i := 0; i < len(validators); i++ {
		validators[i] = &ethpb.Validator{
			ExitEpoch:        params.BeaconConfig().FarFutureEpoch,
			EffectiveBalance: params.BeaconConfig().MaxEffectiveBalance,
		}
		balances[i] = params.BeaconConfig().MaxEffectiveBalance
	}

	atts := []*pbp2p.PendingAttestation{{Data: &ethpb.AttestationData{Target: &ethpb.Checkpoint{}}}}
	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(params.BeaconConfig().SlotsPerEpoch); err != nil {
		t.Fatal(err)
	}
	if err := headState.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	if err := headState.SetBalances(balances); err != nil {
		t.Fatal(err)
	}
	if err := headState.SetPreviousEpochAttestations(atts); err != nil {
		t.Fatal(err)
	}

	b := &ethpb.SignedBeaconBlock{Block: &ethpb.BeaconBlock{Slot: params.BeaconConfig().SlotsPerEpoch}}
	if err := db.SaveBlock(ctx, b); err != nil {
		t.Fatal(err)
	}
	bRoot, err := ssz.HashTreeRoot(b.Block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveState(ctx, headState, bRoot); err != nil {
		t.Fatal(err)
	}

	m := &mock.ChainService{State: headState}
	bs := &Server{
		BeaconDB:             db,
		HeadFetcher:          m,
		ParticipationFetcher: m,
		GenesisTimeFetcher:   &mock.ChainService{},
		StateGen:             stategen.New(db, cache.NewStateSummaryCache()),
	}

	res, err := bs.GetValidatorParticipation(ctx, &ethpb.GetValidatorParticipationRequest{QueryFilter: &ethpb.GetValidatorParticipationRequest_Epoch{Epoch: 0}})
	if err != nil {
		t.Fatal(err)
	}

	wanted := &ethpb.ValidatorParticipation{EligibleEther: validatorCount * params.BeaconConfig().MaxEffectiveBalance}
	if !reflect.DeepEqual(res.Participation, wanted) {
		t.Error("Incorrect validator participation respond")
	}
}

func TestServer_GetValidatorParticipation_DoesntExist(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)
	ctx := context.Background()

	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(params.BeaconConfig().SlotsPerEpoch); err != nil {
		t.Fatal(err)
	}

	b := &ethpb.SignedBeaconBlock{Block: &ethpb.BeaconBlock{Slot: params.BeaconConfig().SlotsPerEpoch}}
	if err := db.SaveBlock(ctx, b); err != nil {
		t.Fatal(err)
	}
	bRoot, err := ssz.HashTreeRoot(b.Block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveState(ctx, headState, bRoot); err != nil {
		t.Fatal(err)
	}

	m := &mock.ChainService{State: headState}
	bs := &Server{
		BeaconDB:             db,
		HeadFetcher:          m,
		ParticipationFetcher: m,
		GenesisTimeFetcher:   &mock.ChainService{},
		StateGen:             stategen.New(db, cache.NewStateSummaryCache()),
	}

	res, err := bs.GetValidatorParticipation(ctx, &ethpb.GetValidatorParticipationRequest{QueryFilter: &ethpb.GetValidatorParticipationRequest_Epoch{Epoch: 0}})
	if err != nil {
		t.Fatal(err)
	}

	if res.Participation.VotedEther != 0 || res.Participation.EligibleEther != 0 {
		t.Error("Incorrect validator participation response")
	}
}

func TestServer_GetValidatorParticipation_FromArchive_FinalizedEpoch(t *testing.T) {
	db := dbTest.SetupDB(t)
	defer dbTest.TeardownDB(t, db)
	ctx := context.Background()
	part := &ethpb.ValidatorParticipation{
		GlobalParticipationRate: 1.0,
		VotedEther:              20,
		EligibleEther:           20,
	}
	epoch := uint64(1)
	// We archive data for epoch 1.
	if err := db.SaveArchivedValidatorParticipation(ctx, epoch, part); err != nil {
		t.Fatal(err)
	}
	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(helpers.StartSlot(epoch + 10)); err != nil {
		t.Fatal(err)
	}
	if err := headState.SetFinalizedCheckpoint(&ethpb.Checkpoint{Epoch: epoch + 5}); err != nil {
		t.Fatal(err)
	}

	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			// 10 epochs into the future.
			State: headState,
		},
	}
	want := &ethpb.ValidatorParticipationResponse{
		Epoch:         epoch,
		Finalized:     true,
		Participation: part,
	}
	// We request epoch 1.
	res, err := bs.GetValidatorParticipation(ctx, &ethpb.GetValidatorParticipationRequest{
		QueryFilter: &ethpb.GetValidatorParticipationRequest_Epoch{
			Epoch: epoch,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(want, res) {
		t.Errorf("Wanted %v, received %v", want, res)
	}
}

func BenchmarkListValidatorBalances(b *testing.B) {
	b.StopTimer()
	db := dbTest.SetupDB(b)
	defer dbTest.TeardownDB(b, db)

	ctx := context.Background()
	count := 1000
	setupValidators(b, db, count)

	headState, err := db.HeadState(ctx)
	if err != nil {
		b.Fatal(err)
	}

	bs := &Server{
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}

	req := &ethpb.ListValidatorBalancesRequest{PageSize: 100}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if _, err := bs.ListValidatorBalances(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListValidatorBalances_FromArchive(b *testing.B) {
	b.StopTimer()
	db := dbTest.SetupDB(b)
	defer dbTest.TeardownDB(b, db)

	ctx := context.Background()
	currentNumValidators := 1000
	numOldBalances := 50
	validators := make([]*ethpb.Validator, currentNumValidators)
	oldBalances := make([]uint64, numOldBalances)
	for i := 0; i < currentNumValidators; i++ {
		validators[i] = &ethpb.Validator{
			PublicKey: []byte(strconv.Itoa(i)),
		}
	}
	for i := 0; i < numOldBalances; i++ {
		oldBalances[i] = params.BeaconConfig().MaxEffectiveBalance
	}
	// We archive old balances for epoch 50.
	if err := db.SaveArchivedBalances(ctx, 50, oldBalances); err != nil {
		b.Fatal(err)
	}
	headState := testutil.NewBeaconState()
	if err := headState.SetSlot(helpers.StartSlot(100 /* epoch 100 */)); err != nil {
		b.Fatal(err)
	}
	if err := headState.SetValidators(validators); err != nil {
		b.Fatal(err)
	}
	bs := &Server{
		BeaconDB: db,
		HeadFetcher: &mock.ChainService{
			State: headState,
		},
	}

	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if _, err := bs.ListValidatorBalances(
			ctx,
			&ethpb.ListValidatorBalancesRequest{
				QueryFilter: &ethpb.ListValidatorBalancesRequest_Epoch{
					Epoch: 50,
				},
				PageSize: 100,
			},
		); err != nil {
			b.Fatal(err)
		}
	}
}

func setupValidators(t testing.TB, db db.Database, count int) ([]*ethpb.Validator, []uint64) {
	ctx := context.Background()
	balances := make([]uint64, count)
	validators := make([]*ethpb.Validator, 0, count)
	for i := 0; i < count; i++ {
		pubKey := pubKey(uint64(i))
		balances[i] = uint64(i)
		validators = append(validators, &ethpb.Validator{
			PublicKey:             pubKey,
			WithdrawalCredentials: make([]byte, 32),
		})
	}
	blk := &ethpb.BeaconBlock{
		Slot: 0,
	}
	blockRoot, err := ssz.HashTreeRoot(blk)
	if err != nil {
		t.Fatal(err)
	}
	s := testutil.NewBeaconState()
	if err := s.SetValidators(validators); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBalances(balances); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveState(
		context.Background(),
		s,
		blockRoot,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveHeadBlockRoot(ctx, blockRoot); err != nil {
		t.Fatal(err)
	}
	return validators, balances
}
