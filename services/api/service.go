// Package api contains the API webserver for the proposer and block-builder APIs
package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NYTimes/gziphandler"
	builderCapella "github.com/attestantio/go-builder-client/api/capella"
	"github.com/attestantio/go-eth2-client/api/v1/capella"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/buger/jsonparser"
	"github.com/flashbots/go-boost-utils/bls"
	boostTypes "github.com/flashbots/go-boost-utils/types"
	"github.com/flashbots/go-utils/cli"
	"github.com/flashbots/go-utils/httplogger"
	"github.com/flashbots/mev-boost-relay/beaconclient"
	"github.com/flashbots/mev-boost-relay/common"
	"github.com/flashbots/mev-boost-relay/database"
	"github.com/flashbots/mev-boost-relay/datastore"
	"github.com/go-redis/redis/v9"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	uberatomic "go.uber.org/atomic"
	"golang.org/x/exp/slices"
)

const (
	ErrBlockAlreadyKnown  = "simulation failed: block already known"
	ErrBlockRequiresReorg = "simulation failed: block requires a reorg"
	ErrMissingTrieNode    = "missing trie node"
)

var (
	ErrMissingLogOpt              = errors.New("log parameter is nil")
	ErrMissingBeaconClientOpt     = errors.New("beacon-client is nil")
	ErrMissingDatastoreOpt        = errors.New("proposer datastore is nil")
	ErrRelayPubkeyMismatch        = errors.New("relay pubkey does not match existing one")
	ErrServerAlreadyStarted       = errors.New("server was already started")
	ErrBuilderAPIWithoutSecretKey = errors.New("cannot start builder API without secret key")
	ErrMismatchedForkVersions     = errors.New("can not find matching fork versions as retrieved from beacon node")
	ErrMissingForkVersions        = errors.New("invalid bellatrix/capella fork version from beacon node")
)

var (
	// Proposer API (builder-specs)
	pathStatus            = "/eth/v1/builder/status"
	pathRegisterValidator = "/eth/v1/builder/validators"
	pathGetHeader         = "/eth/v1/builder/header/{slot:[0-9]+}/{parent_hash:0x[a-fA-F0-9]+}/{pubkey:0x[a-fA-F0-9]+}"
	pathGetPayload        = "/eth/v1/builder/blinded_blocks"

	// Block builder API
	pathBuilderGetValidators = "/relay/v1/builder/validators"
	pathSubmitNewBlock       = "/relay/v1/builder/blocks"

	// Data API
	pathDataProposerPayloadDelivered = "/relay/v1/data/bidtraces/proposer_payload_delivered"
	pathDataBuilderBidsReceived      = "/relay/v1/data/bidtraces/builder_blocks_received"
	pathDataValidatorRegistration    = "/relay/v1/data/validator_registration"

	// Internal API
	pathInternalBuilderStatus     = "/internal/v1/builder/{pubkey:0x[a-fA-F0-9]+}"
	pathInternalBuilderCollateral = "/internal/v1/builder/collateral/{pubkey:0x[a-fA-F0-9]+}"

	// number of goroutines to save active validator
	numActiveValidatorProcessors = cli.GetEnvInt("NUM_ACTIVE_VALIDATOR_PROCESSORS", 10)
	numValidatorRegProcessors    = cli.GetEnvInt("NUM_VALIDATOR_REG_PROCESSORS", 10)

	// various timings
	timeoutGetPayloadRetryMs  = cli.GetEnvInt("GETPAYLOAD_RETRY_TIMEOUT_MS", 100)
	getPayloadRequestCutoffMs = cli.GetEnvInt("GETPAYLOAD_REQUEST_CUTOFF_MS", 4000)
	getPayloadResponseDelayMs = cli.GetEnvInt("GETPAYLOAD_RESPONSE_DELAY_MS", 1000)

	// api settings
	apiReadTimeoutMs       = cli.GetEnvInt("API_TIMEOUT_READ_MS", 1500)
	apiReadHeaderTimeoutMs = cli.GetEnvInt("API_TIMEOUT_READHEADER_MS", 600)
	apiWriteTimeoutMs      = cli.GetEnvInt("API_TIMEOUT_WRITE_MS", 10000)
	apiIdleTimeoutMs       = cli.GetEnvInt("API_TIMEOUT_IDLE_MS", 3000)
	apiMaxHeaderBytes      = cli.GetEnvInt("API_MAX_HEADER_BYTES", 60000)

	// user-agents which shouldn't receive bids
	apiNoHeaderUserAgents = common.GetEnvStrSlice("NO_HEADER_USERAGENTS", []string{
		"mev-boost/v1.5.0 Go-http-client/1.1", // Prysm v4.0.1 (Shapella signing issue)
	})
)

// RelayAPIOpts contains the options for a relay
type RelayAPIOpts struct {
	Log *logrus.Entry

	ListenAddr  string
	BlockSimURL string

	BeaconClient beaconclient.IMultiBeaconClient
	Datastore    *datastore.Datastore
	Redis        *datastore.RedisCache
	Memcached    *datastore.Memcached
	DB           database.IDatabaseService

	SecretKey *bls.SecretKey // used to sign bids (getHeader responses)

	// Network specific variables
	EthNetDetails common.EthNetworkDetails

	// APIs to enable
	ProposerAPI     bool
	BlockBuilderAPI bool
	DataAPI         bool
	PprofAPI        bool
	InternalAPI     bool
}

type payloadAttributesHelper struct {
	slot              uint64
	parentHash        string
	withdrawalsRoot   phase0.Root
	payloadAttributes beaconclient.PayloadAttributes
}

// Data needed to issue a block validation request.
type blockSimOptions struct {
	isHighPrio bool
	fastTrack  bool
	log        *logrus.Entry
	builder    *blockBuilderCacheEntry
	req        *common.BuilderBlockValidationRequest
}

type blockBuilderCacheEntry struct {
	status     common.BuilderStatus
	collateral *big.Int
}

// RelayAPI represents a single Relay instance
type RelayAPI struct {
	opts RelayAPIOpts
	log  *logrus.Entry

	blsSk     *bls.SecretKey
	publicKey *boostTypes.PublicKey

	srv        *http.Server
	srvStarted uberatomic.Bool

	beaconClient beaconclient.IMultiBeaconClient
	datastore    *datastore.Datastore
	redis        *datastore.RedisCache
	memcached    *datastore.Memcached
	db           database.IDatabaseService

	headSlot       uberatomic.Uint64
	genesisInfo    *beaconclient.GetGenesisResponse
	bellatrixEpoch uint64
	capellaEpoch   uint64

	proposerDutiesLock       sync.RWMutex
	proposerDutiesResponse   *[]byte // raw http response
	proposerDutiesMap        map[uint64]*common.BuilderGetValidatorsResponseEntry
	proposerDutiesSlot       uint64
	isUpdatingProposerDuties uberatomic.Bool

	blockSimRateLimiter IBlockSimRateLimiter

	activeValidatorC chan boostTypes.PubkeyHex
	validatorRegC    chan boostTypes.SignedValidatorRegistration

	// used to wait on any active getPayload calls on shutdown
	getPayloadCallsInFlight sync.WaitGroup

	// Feature flags
	ffForceGetHeader204          bool
	ffDisableLowPrioBuilders     bool
	ffDisablePayloadDBStorage    bool // disable storing the execution payloads in the database
	ffLogInvalidSignaturePayload bool // log payload if getPayload signature validation fails
	ffEnableCancellations        bool // whether to enable block builder cancellations
	ffRegValContinueOnInvalidSig bool // whether to continue processing further validators if one fails

	payloadAttributes     map[string]payloadAttributesHelper // key:parentBlockHash
	payloadAttributesLock sync.RWMutex

	// The slot we are currently optimistically simulating.
	optimisticSlot uberatomic.Uint64
	// The number of optimistic blocks being processed (only used for logging).
	optimisticBlocksInFlight uberatomic.Uint64
	// Wait group used to monitor status of per-slot optimistic processing.
	optimisticBlocksWG sync.WaitGroup
	// Cache for builder statuses and collaterals.
	blockBuildersCache map[string]*blockBuilderCacheEntry
}

// NewRelayAPI creates a new service. if builders is nil, allow any builder
func NewRelayAPI(opts RelayAPIOpts) (api *RelayAPI, err error) {
	if opts.Log == nil {
		return nil, ErrMissingLogOpt
	}

	if opts.BeaconClient == nil {
		return nil, ErrMissingBeaconClientOpt
	}

	if opts.Datastore == nil {
		return nil, ErrMissingDatastoreOpt
	}

	// If block-builder API is enabled, then ensure secret key is all set
	var publicKey boostTypes.PublicKey
	if opts.BlockBuilderAPI {
		if opts.SecretKey == nil {
			return nil, ErrBuilderAPIWithoutSecretKey
		}

		// If using a secret key, ensure it's the correct one
		blsPubkey, err := bls.PublicKeyFromSecretKey(opts.SecretKey)
		if err != nil {
			return nil, err
		}
		publicKey, err = boostTypes.BlsPublicKeyToPublicKey(blsPubkey)
		if err != nil {
			return nil, err
		}
		opts.Log.Infof("Using BLS key: %s", publicKey.String())

		// ensure pubkey is same across all relay instances
		_pubkey, err := opts.Redis.GetRelayConfig(datastore.RedisConfigFieldPubkey)
		if err != nil {
			return nil, err
		} else if _pubkey == "" {
			err := opts.Redis.SetRelayConfig(datastore.RedisConfigFieldPubkey, publicKey.String())
			if err != nil {
				return nil, err
			}
		} else if _pubkey != publicKey.String() {
			return nil, fmt.Errorf("%w: new=%s old=%s", ErrRelayPubkeyMismatch, publicKey.String(), _pubkey)
		}
	}

	api = &RelayAPI{
		opts:         opts,
		log:          opts.Log,
		blsSk:        opts.SecretKey,
		publicKey:    &publicKey,
		datastore:    opts.Datastore,
		beaconClient: opts.BeaconClient,
		redis:        opts.Redis,
		memcached:    opts.Memcached,
		db:           opts.DB,

		payloadAttributes: make(map[string]payloadAttributesHelper),

		proposerDutiesResponse: &[]byte{},
		blockSimRateLimiter:    NewBlockSimulationRateLimiter(opts.BlockSimURL),

		activeValidatorC: make(chan boostTypes.PubkeyHex, 450_000),
		validatorRegC:    make(chan boostTypes.SignedValidatorRegistration, 450_000),
	}

	if os.Getenv("FORCE_GET_HEADER_204") == "1" {
		api.log.Warn("env: FORCE_GET_HEADER_204 - forcing getHeader to always return 204")
		api.ffForceGetHeader204 = true
	}

	if os.Getenv("DISABLE_LOWPRIO_BUILDERS") == "1" {
		api.log.Warn("env: DISABLE_LOWPRIO_BUILDERS - allowing only high-level builders")
		api.ffDisableLowPrioBuilders = true
	}

	if os.Getenv("DISABLE_PAYLOAD_DATABASE_STORAGE") == "1" {
		api.log.Warn("env: DISABLE_PAYLOAD_DATABASE_STORAGE - disabling storing payloads in the database")
		api.ffDisablePayloadDBStorage = true
	}

	if os.Getenv("LOG_INVALID_GETPAYLOAD_SIGNATURE") == "1" {
		api.log.Warn("env: LOG_INVALID_GETPAYLOAD_SIGNATURE - getPayload payloads with invalid proposer signature will be logged")
		api.ffLogInvalidSignaturePayload = true
	}

	if os.Getenv("ENABLE_BUILDER_CANCELLATIONS") == "1" {
		api.log.Warn("env: ENABLE_BUILDER_CANCELLATIONS - builders are allowed to cancel submissions when using ?cancellation=1")
		api.ffEnableCancellations = true
	}

	if os.Getenv("REGISTER_VALIDATOR_CONTINUE_ON_INVALID_SIG") == "1" {
		api.log.Warn("env: REGISTER_VALIDATOR_CONTINUE_ON_INVALID_SIG - validator registration will continue processing even if one validator has an invalid signature")
		api.ffRegValContinueOnInvalidSig = true
	}

	return api, nil
}

func (api *RelayAPI) getRouter() http.Handler {
	r := mux.NewRouter()

	r.HandleFunc("/", api.handleRoot).Methods(http.MethodGet)

	// Proposer API
	if api.opts.ProposerAPI {
		api.log.Info("proposer API enabled")
		r.HandleFunc(pathStatus, api.handleStatus).Methods(http.MethodGet)
		r.HandleFunc(pathRegisterValidator, api.handleRegisterValidator).Methods(http.MethodPost)
		r.HandleFunc(pathGetHeader, api.handleGetHeader).Methods(http.MethodGet)
		r.HandleFunc(pathGetPayload, api.handleGetPayload).Methods(http.MethodPost)
	}

	// Builder API
	if api.opts.BlockBuilderAPI {
		api.log.Info("block builder API enabled")
		r.HandleFunc(pathBuilderGetValidators, api.handleBuilderGetValidators).Methods(http.MethodGet)
		r.HandleFunc(pathSubmitNewBlock, api.handleSubmitNewBlock).Methods(http.MethodPost)
	}

	// Data API
	if api.opts.DataAPI {
		api.log.Info("data API enabled")
		r.HandleFunc(pathDataProposerPayloadDelivered, api.handleDataProposerPayloadDelivered).Methods(http.MethodGet)
		r.HandleFunc(pathDataBuilderBidsReceived, api.handleDataBuilderBidsReceived).Methods(http.MethodGet)
		r.HandleFunc(pathDataValidatorRegistration, api.handleDataValidatorRegistration).Methods(http.MethodGet)
	}

	// Pprof
	if api.opts.PprofAPI {
		api.log.Info("pprof API enabled")
		r.PathPrefix("/debug/pprof/").Handler(http.DefaultServeMux)
	}

	// /internal/...
	if api.opts.InternalAPI {
		api.log.Info("internal API enabled")
		r.HandleFunc(pathInternalBuilderStatus, api.handleInternalBuilderStatus).Methods(http.MethodGet, http.MethodPost, http.MethodPut)
		r.HandleFunc(pathInternalBuilderCollateral, api.handleInternalBuilderCollateral).Methods(http.MethodPost, http.MethodPut)
	}

	// r.Use(mux.CORSMethodMiddleware(r))
	loggedRouter := httplogger.LoggingMiddlewareLogrus(api.log, r)
	withGz := gziphandler.GzipHandler(loggedRouter)
	return withGz
}

func (api *RelayAPI) isCapella(slot uint64) bool {
	if api.capellaEpoch == 0 { // CL didn't yet have it
		return false
	}
	epoch := slot / common.SlotsPerEpoch
	return epoch >= api.capellaEpoch
}

func (api *RelayAPI) isBellatrix(slot uint64) bool {
	return !api.isCapella(slot)
}

// StartServer starts the HTTP server for this instance
func (api *RelayAPI) StartServer() (err error) {
	if api.srvStarted.Swap(true) {
		return ErrServerAlreadyStarted
	}

	// Get best beacon-node status by head slot, process current slot and start slot updates
	bestSyncStatus, err := api.beaconClient.BestSyncStatus()
	if err != nil {
		return err
	}

	// Initialize block builder cache.
	api.blockBuildersCache = make(map[string]*blockBuilderCacheEntry)

	// Helpers
	currentSlot := bestSyncStatus.HeadSlot
	currentEpoch := currentSlot / common.SlotsPerEpoch

	api.genesisInfo, err = api.beaconClient.GetGenesis()
	if err != nil {
		return err
	}
	api.log.Infof("genesis info: %d", api.genesisInfo.Data.GenesisTime)

	forkSchedule, err := api.beaconClient.GetForkSchedule()
	if err != nil {
		return err
	}

	// Parse forkSchedule
	for _, fork := range forkSchedule.Data {
		api.log.Infof("forkSchedule: version=%s / epoch=%d", fork.CurrentVersion, fork.Epoch)
		switch fork.CurrentVersion {
		case api.opts.EthNetDetails.BellatrixForkVersionHex:
			api.bellatrixEpoch = fork.Epoch
		case api.opts.EthNetDetails.CapellaForkVersionHex:
			api.capellaEpoch = fork.Epoch
		}
	}

	// Print fork version information
	if api.isCapella(currentSlot) {
		api.log.Infof("capella fork detected (currentEpoch: %d / bellatrixEpoch: %d / capellaEpoch: %d)", currentEpoch, api.bellatrixEpoch, api.capellaEpoch)
	} else if api.isBellatrix(currentSlot) {
		api.log.Infof("bellatrix fork detected (currentEpoch: %d / bellatrixEpoch: %d / capellaEpoch: %d)", currentEpoch, api.bellatrixEpoch, api.capellaEpoch)
		if api.capellaEpoch == 0 {
			api.log.Infof("no capella fork scheduled. update your beacon-node in time.")
		}
	} else {
		return ErrMismatchedForkVersions
	}

	// start things for the block-builder API
	if api.opts.BlockBuilderAPI {
		// Get current proposer duties blocking before starting, to have them ready
		api.updateProposerDuties(bestSyncStatus.HeadSlot)
	}

	// start things specific for the proposer API
	if api.opts.ProposerAPI {
		// Update list of known validators, and start refresh loop
		go api.startKnownValidatorUpdates()

		// Start the worker pool to process active validators
		api.log.Infof("starting %d active validator processors", numActiveValidatorProcessors)
		for i := 0; i < numActiveValidatorProcessors; i++ {
			go api.startActiveValidatorProcessor()
		}

		// Start the validator registration db-save processor
		api.log.Infof("starting %d validator registration processors", numValidatorRegProcessors)
		for i := 0; i < numValidatorRegProcessors; i++ {
			go api.startValidatorRegistrationDBProcessor()
		}
	}

	// Process current slot
	api.processNewSlot(bestSyncStatus.HeadSlot)

	// Start regular slot updates
	go func() {
		c := make(chan beaconclient.HeadEventData)
		api.beaconClient.SubscribeToHeadEvents(c)
		for {
			headEvent := <-c
			api.processNewSlot(headEvent.Slot)
		}
	}()

	// Start regular payload attributes updates only if builder-api is enabled
	// and if using see subscriptions instead of querying for payload attributes
	if api.opts.BlockBuilderAPI {
		go func() {
			c := make(chan beaconclient.PayloadAttributesEvent)
			api.beaconClient.SubscribeToPayloadAttributesEvents(c)
			for {
				payloadAttributes := <-c
				api.processPayloadAttributes(payloadAttributes)
			}
		}()
	}

	api.srv = &http.Server{
		Addr:    api.opts.ListenAddr,
		Handler: api.getRouter(),

		ReadTimeout:       time.Duration(apiReadTimeoutMs) * time.Millisecond,
		ReadHeaderTimeout: time.Duration(apiReadHeaderTimeoutMs) * time.Millisecond,
		WriteTimeout:      time.Duration(apiWriteTimeoutMs) * time.Millisecond,
		IdleTimeout:       time.Duration(apiIdleTimeoutMs) * time.Millisecond,
		MaxHeaderBytes:    apiMaxHeaderBytes,
	}

	err = api.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// StopServer disables sending any bids on getHeader calls, waits a few seconds to catch any remaining getPayload call, and then shuts down the webserver
func (api *RelayAPI) StopServer() (err error) {
	api.log.Info("Stopping server...")

	if api.opts.ProposerAPI {
		// stop sending bids
		api.ffForceGetHeader204 = true
		api.log.Info("Disabled sending bids, waiting a few seconds...")

		// wait a few seconds, for any pending getPayload call to complete
		time.Sleep(5 * time.Second)

		// wait for any active getPayload call to finish
		api.getPayloadCallsInFlight.Wait()
	}

	// shutdown
	return api.srv.Shutdown(context.Background())
}

// startActiveValidatorProcessor keeps listening on the channel and saving active validators to redis
func (api *RelayAPI) startActiveValidatorProcessor() {
	for pubkey := range api.activeValidatorC {
		err := api.redis.SetActiveValidator(pubkey)
		if err != nil {
			api.log.WithError(err).Infof("error setting active validator")
		}
	}
}

// startActiveValidatorProcessor keeps listening on the channel and saving active validators to redis
func (api *RelayAPI) startValidatorRegistrationDBProcessor() {
	for valReg := range api.validatorRegC {
		err := api.datastore.SaveValidatorRegistration(valReg)
		if err != nil {
			api.log.WithError(err).WithFields(logrus.Fields{
				"reg_pubkey":       valReg.Message.Pubkey,
				"reg_feeRecipient": valReg.Message.FeeRecipient,
				"reg_gasLimit":     valReg.Message.GasLimit,
				"reg_timestamp":    valReg.Message.Timestamp,
			}).Error("error saving validator registration")
		}
	}
}

// simulateBlock sends a request for a block simulation to blockSimRateLimiter.
func (api *RelayAPI) simulateBlock(ctx context.Context, opts blockSimOptions) (requestErr, validationErr error) {
	t := time.Now()
	requestErr, validationErr = api.blockSimRateLimiter.Send(ctx, opts.req, opts.isHighPrio, opts.fastTrack)
	log := opts.log.WithFields(logrus.Fields{
		"durationMs": time.Since(t).Milliseconds(),
		"numWaiting": api.blockSimRateLimiter.CurrentCounter(),
	})
	if validationErr != nil {
		// TODO(mikeneuder): consider the negation logic here if it improves readability.
		ignoreError := validationErr.Error() == ErrBlockAlreadyKnown || validationErr.Error() == ErrBlockRequiresReorg || strings.Contains(validationErr.Error(), ErrMissingTrieNode)
		if !ignoreError {
			log.WithError(validationErr).Warn("block validation failed")
			return nil, validationErr
		}
		log.WithError(validationErr).Warn("block validation failed with ignorable error")
		return nil, nil
	}
	if requestErr != nil {
		log.WithError(requestErr).Warn("block validation failed: request error")
		return requestErr, nil
	}
	log.Info("block validation successful")
	return nil, nil
}

func (api *RelayAPI) demoteBuilder(pubkey string, req *common.BuilderSubmitBlockRequest, simError error) {
	builderEntry, ok := api.blockBuildersCache[pubkey]
	if !ok {
		api.log.Warnf("builder %v not in the builder cache", pubkey)
		builderEntry = &blockBuilderCacheEntry{} //nolint:exhaustruct
	}
	newStatus := common.BuilderStatus{
		IsHighPrio:    builderEntry.status.IsHighPrio,
		IsBlacklisted: builderEntry.status.IsBlacklisted,
		IsOptimistic:  false,
	}
	api.log.Infof("demoted builder, new status: %v", newStatus)
	if err := api.db.SetBlockBuilderIDStatusIsOptimistic(pubkey, false); err != nil {
		api.log.Error(fmt.Errorf("error setting builder: %v status: %w", pubkey, err))
	}
	// Write to demotions table.
	api.log.WithFields(logrus.Fields{"builder_pubkey": pubkey}).Info("demoting builder")
	if err := api.db.InsertBuilderDemotion(req, simError); err != nil {
		api.log.WithError(err).WithFields(logrus.Fields{
			"errorWritingDemotionToDB": true,
			"bidTrace":                 req.Message,
			"simError":                 simError,
		}).Error("failed to save demotion to database")
	}
}

// processOptimisticBlock is called on a new goroutine when a optimistic block
// needs to be simulated.
func (api *RelayAPI) processOptimisticBlock(opts blockSimOptions) {
	api.optimisticBlocksInFlight.Add(1)
	defer func() { api.optimisticBlocksInFlight.Sub(1) }()
	api.optimisticBlocksWG.Add(1)
	defer api.optimisticBlocksWG.Done()

	ctx := context.Background()
	builderPubkey := opts.req.BuilderPubkey().String()
	opts.log.WithFields(logrus.Fields{
		"builderPubkey": builderPubkey,
		// NOTE: this value is just an estimate because many goroutines could be
		// updating api.optimisticBlocksInFlight concurrently. Since we just use
		// it for logging, it is not atomic to avoid the performance impact.
		"optBlocksInFlight": api.optimisticBlocksInFlight,
	}).Infof("simulating optimistic block with hash: %v", opts.req.BuilderSubmitBlockRequest.BlockHash())
	reqErr, simErr := api.simulateBlock(ctx, opts)
	if reqErr != nil || simErr != nil {
		// Mark builder as non-optimistic.
		opts.builder.status.IsOptimistic = false
		api.log.WithError(simErr).Warn("block simulation failed in processOptimisticBlock, demoting builder")

		// Demote the builder.
		api.demoteBuilder(builderPubkey, &opts.req.BuilderSubmitBlockRequest, simErr)
	}
}

func (api *RelayAPI) processPayloadAttributes(payloadAttributes beaconclient.PayloadAttributesEvent) {
	apiHeadSlot := api.headSlot.Load()
	payloadAttrSlot := payloadAttributes.Data.ProposalSlot

	// require proposal slot in the future
	if payloadAttrSlot <= apiHeadSlot {
		return
	}
	log := api.log.WithFields(logrus.Fields{
		"headSlot":          apiHeadSlot,
		"payloadAttrSlot":   payloadAttrSlot,
		"payloadAttrParent": payloadAttributes.Data.ParentBlockHash,
	})

	// discard payload attributes if already known
	api.payloadAttributesLock.RLock()
	_, ok := api.payloadAttributes[payloadAttributes.Data.ParentBlockHash]
	api.payloadAttributesLock.RUnlock()

	if ok {
		return
	}

	var withdrawalsRoot phase0.Root
	var err error
	if api.isCapella(payloadAttrSlot) {
		withdrawalsRoot, err = ComputeWithdrawalsRoot(payloadAttributes.Data.PayloadAttributes.Withdrawals)
		log = log.WithField("withdrawalsRoot", withdrawalsRoot.String())
		if err != nil {
			log.WithError(err).Error("error computing withdrawals root")
			return
		}
	}

	api.payloadAttributesLock.Lock()
	defer api.payloadAttributesLock.Unlock()

	// Step 1: clean up old ones
	for parentBlockHash, attr := range api.payloadAttributes {
		if attr.slot < apiHeadSlot {
			delete(api.payloadAttributes, parentBlockHash)
		}
	}

	// Step 2: save new one
	api.payloadAttributes[payloadAttributes.Data.ParentBlockHash] = payloadAttributesHelper{
		slot:              payloadAttrSlot,
		parentHash:        payloadAttributes.Data.ParentBlockHash,
		withdrawalsRoot:   withdrawalsRoot,
		payloadAttributes: payloadAttributes.Data.PayloadAttributes,
	}

	log.WithFields(logrus.Fields{
		"randao":    payloadAttributes.Data.PayloadAttributes.PrevRandao,
		"timestamp": payloadAttributes.Data.PayloadAttributes.Timestamp,
	}).Info("updated payload attributes")
}

func (api *RelayAPI) processNewSlot(headSlot uint64) {
	prevHeadSlot := api.headSlot.Load()
	if headSlot <= prevHeadSlot {
		return
	}

	// If there's gaps between previous and new headslot, print the missed slots
	if prevHeadSlot > 0 {
		for s := prevHeadSlot + 1; s < headSlot; s++ {
			api.log.WithField("missedSlot", s).Warnf("missed slot: %d", s)
		}
	}

	// store the head slot
	api.headSlot.Store(headSlot)

	// only for builder-api
	if api.opts.BlockBuilderAPI || api.opts.ProposerAPI {
		// update proposer duties in the background
		go api.updateProposerDuties(headSlot)

		// update the optimistic slot
		go api.prepareBuildersForSlot(headSlot)
	}

	// log
	epoch := headSlot / common.SlotsPerEpoch
	api.log.WithFields(logrus.Fields{
		"epoch":              epoch,
		"slotHead":           headSlot,
		"slotStartNextEpoch": (epoch + 1) * common.SlotsPerEpoch,
	}).Infof("updated headSlot to %d", headSlot)

	if api.isBellatrix(prevHeadSlot) && api.isCapella(headSlot) {
		api.log.Info("====================== NOW ON CAPELLA ======================")
	}
}

func (api *RelayAPI) updateProposerDuties(headSlot uint64) {
	// Ensure only one updating is running at a time
	if api.isUpdatingProposerDuties.Swap(true) {
		return
	}
	defer api.isUpdatingProposerDuties.Store(false)

	// Update once every 8 slots (or more, if a slot was missed)
	if headSlot%8 != 0 && headSlot-api.proposerDutiesSlot < 8 {
		return
	}

	// Load upcoming proposer duties from Redis
	duties, err := api.redis.GetProposerDuties()
	if err != nil {
		api.log.WithError(err).Error("failed getting proposer duties from redis")
		return
	}

	// Prepare raw bytes for HTTP response
	respBytes, err := json.Marshal(duties)
	if err != nil {
		api.log.WithError(err).Error("error marshalling duties")
	}

	// Prepare the map for lookup by slot
	dutiesMap := make(map[uint64]*common.BuilderGetValidatorsResponseEntry)
	for index, duty := range duties {
		dutiesMap[duty.Slot] = &duties[index]
	}

	// Update
	api.proposerDutiesLock.Lock()
	if len(respBytes) > 0 {
		api.proposerDutiesResponse = &respBytes
	}
	api.proposerDutiesMap = dutiesMap
	api.proposerDutiesSlot = headSlot
	api.proposerDutiesLock.Unlock()

	// pretty-print
	_duties := make([]string, len(duties))
	for i, duty := range duties {
		_duties[i] = fmt.Sprint(duty.Slot)
	}
	sort.Strings(_duties)
	api.log.Infof("proposer duties updated: %s", strings.Join(_duties, ", "))
}

func (api *RelayAPI) prepareBuildersForSlot(headSlot uint64) {
	// Wait until there are no optimistic blocks being processed. Then we can
	// safely update the slot.
	api.optimisticBlocksWG.Wait()
	api.optimisticSlot.Store(headSlot + 1)

	builders, err := api.db.GetBlockBuilders()
	if err != nil {
		api.log.WithError(err).Error("unable to read block builders from db, not updating builder cache")
		return
	}
	api.log.Debugf("Updating builder cache with %d builders from database", len(builders))

	newCache := make(map[string]*blockBuilderCacheEntry)
	for _, v := range builders {
		entry := &blockBuilderCacheEntry{ //nolint:exhaustruct
			status: common.BuilderStatus{
				IsHighPrio:    v.IsHighPrio,
				IsBlacklisted: v.IsBlacklisted,
				IsOptimistic:  v.IsOptimistic,
			},
		}
		// Try to parse builder collateral string to big int.
		builderCollateral, ok := big.NewInt(0).SetString(v.Collateral, 10)
		if !ok {
			api.log.WithError(err).Errorf("could not parse builder collateral string %s", v.Collateral)
			entry.collateral = big.NewInt(0)
		} else {
			entry.collateral = builderCollateral
		}
		newCache[v.BuilderPubkey] = entry
	}
	api.blockBuildersCache = newCache
}

func (api *RelayAPI) startKnownValidatorUpdates() {
	for {
		// Refresh known validators
		cnt, err := api.datastore.RefreshKnownValidators()
		if err != nil {
			api.log.WithError(err).Error("error getting known validators")
		} else {
			api.log.WithField("cnt", cnt).Info("updated known validators")
		}

		// Wait for one epoch (at the beginning, because initially the validators have already been queried)
		time.Sleep(common.DurationPerEpoch / 2)
	}
}

func (api *RelayAPI) RespondError(w http.ResponseWriter, code int, message string) {
	api.Respond(w, code, HTTPErrorResp{code, message})
}

func (api *RelayAPI) RespondOK(w http.ResponseWriter, response any) {
	api.Respond(w, http.StatusOK, response)
}

func (api *RelayAPI) RespondMsg(w http.ResponseWriter, code int, msg string) {
	api.Respond(w, code, struct{ message string }{message: msg})
}

func (api *RelayAPI) Respond(w http.ResponseWriter, code int, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		api.log.WithField("response", response).WithError(err).Error("Couldn't write response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (api *RelayAPI) handleStatus(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// ---------------
//  PROPOSER APIS
// ---------------

func (api *RelayAPI) handleRoot(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "MEV-Boost Relay API")
}

func (api *RelayAPI) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	ua := req.UserAgent()
	log := api.log.WithFields(logrus.Fields{
		"method":        "registerValidator",
		"ua":            ua,
		"mevBoostV":     common.GetMevBoostVersionFromUserAgent(ua),
		"headSlot":      api.headSlot.Load(),
		"contentLength": req.ContentLength,
	})

	start := time.Now().UTC()
	registrationTimestampUpperBound := start.Unix() + 10 // 10 seconds from now

	numRegTotal := 0
	numRegProcessed := 0
	numRegActive := 0
	numRegNew := 0
	processingStoppedByError := false

	// Setup error handling
	handleError := func(_log *logrus.Entry, code int, msg string) {
		processingStoppedByError = true
		_log.Warnf("error: %s", msg)
		api.RespondError(w, code, msg)
	}

	// Start processing
	if req.ContentLength == 0 {
		log.Info("empty request")
		api.RespondError(w, http.StatusBadRequest, "empty request")
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.WithError(err).WithField("contentLength", req.ContentLength).Warn("failed to read request body")
		api.RespondError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	req.Body.Close()

	parseRegistration := func(value []byte) (reg *boostTypes.SignedValidatorRegistration, err error) {
		// Pubkey
		_pubkey, err := jsonparser.GetUnsafeString(value, "message", "pubkey")
		if err != nil {
			return nil, fmt.Errorf("registration message error (pubkey): %w", err)
		}

		pubkey, err := boostTypes.HexToPubkey(_pubkey)
		if err != nil {
			return nil, fmt.Errorf("registration message error (pubkey): %w", err)
		}

		// Timestamp
		_timestamp, err := jsonparser.GetUnsafeString(value, "message", "timestamp")
		if err != nil {
			return nil, fmt.Errorf("registration message error (timestamp): %w", err)
		}

		timestamp, err := strconv.ParseUint(_timestamp, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid timestamp: %w", err)
		}

		// GasLimit
		_gasLimit, err := jsonparser.GetUnsafeString(value, "message", "gas_limit")
		if err != nil {
			return nil, fmt.Errorf("registration message error (gasLimit): %w", err)
		}

		gasLimit, err := strconv.ParseUint(_gasLimit, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid gasLimit: %w", err)
		}

		// FeeRecipient
		_feeRecipient, err := jsonparser.GetUnsafeString(value, "message", "fee_recipient")
		if err != nil {
			return nil, fmt.Errorf("registration message error (fee_recipient): %w", err)
		}

		feeRecipient, err := boostTypes.HexToAddress(_feeRecipient)
		if err != nil {
			return nil, fmt.Errorf("registration message error (fee_recipient): %w", err)
		}

		// Signature
		_signature, err := jsonparser.GetUnsafeString(value, "signature")
		if err != nil {
			return nil, fmt.Errorf("registration message error (signature): %w", err)
		}

		signature, err := boostTypes.HexToSignature(_signature)
		if err != nil {
			return nil, fmt.Errorf("registration message error (signature): %w", err)
		}

		// Construct and return full registration object
		reg = &boostTypes.SignedValidatorRegistration{
			Message: &boostTypes.RegisterValidatorRequestMessage{
				FeeRecipient: feeRecipient,
				GasLimit:     gasLimit,
				Timestamp:    timestamp,
				Pubkey:       pubkey,
			},
			Signature: signature,
		}

		return reg, nil
	}

	// Iterate over the registrations
	_, err = jsonparser.ArrayEach(body, func(value []byte, dataType jsonparser.ValueType, offset int, _err error) {
		numRegTotal += 1
		if processingStoppedByError {
			return
		}
		numRegProcessed += 1
		regLog := log.WithFields(logrus.Fields{
			"numRegistrationsSoFar":     numRegTotal,
			"numRegistrationsProcessed": numRegProcessed,
		})

		// Extract immediately necessary registration fields
		signedValidatorRegistration, err := parseRegistration(value)
		if err != nil {
			handleError(regLog, http.StatusBadRequest, err.Error())
			return
		}

		// Add validator pubkey to logs
		pkHex := signedValidatorRegistration.Message.Pubkey.PubkeyHex()
		regLog = regLog.WithFields(logrus.Fields{
			"pubkey":       pkHex,
			"signature":    signedValidatorRegistration.Signature.String(),
			"feeRecipient": signedValidatorRegistration.Message.FeeRecipient.String(),
			"gasLimit":     signedValidatorRegistration.Message.GasLimit,
			"timestamp":    signedValidatorRegistration.Message.Timestamp,
		})

		// Ensure a valid timestamp (not too early, and not too far in the future)
		registrationTimestamp := int64(signedValidatorRegistration.Message.Timestamp)
		if registrationTimestamp < int64(api.genesisInfo.Data.GenesisTime) {
			handleError(regLog, http.StatusBadRequest, "timestamp too early")
			return
		} else if registrationTimestamp > registrationTimestampUpperBound {
			handleError(regLog, http.StatusBadRequest, "timestamp too far in the future")
			return
		}

		// Check if a real validator
		isKnownValidator := api.datastore.IsKnownValidator(pkHex)
		if !isKnownValidator {
			handleError(regLog, http.StatusBadRequest, fmt.Sprintf("not a known validator: %s", pkHex.String()))
			return
		}

		// Keep track of active validators
		numRegActive += 1
		select {
		case api.activeValidatorC <- pkHex:
		default:
			regLog.Error("active validator channel full")
		}

		// Check for a previous registration timestamp
		prevTimestamp, err := api.redis.GetValidatorRegistrationTimestamp(pkHex)
		if err != nil {
			regLog.WithError(err).Error("error getting last registration timestamp")
		} else if prevTimestamp >= signedValidatorRegistration.Message.Timestamp {
			// abort if the current registration timestamp is older or equal to the last known one
			return
		}

		// Verify the signature
		ok, err := boostTypes.VerifySignature(signedValidatorRegistration.Message, api.opts.EthNetDetails.DomainBuilder, signedValidatorRegistration.Message.Pubkey[:], signedValidatorRegistration.Signature[:])
		if err != nil {
			regLog.WithError(err).Error("error verifying registerValidator signature")
			return
		} else if !ok {
			regLog.Info("invalid validator signature")
			if api.ffRegValContinueOnInvalidSig {
				return
			} else {
				handleError(regLog, http.StatusBadRequest, fmt.Sprintf("failed to verify validator signature for %s", signedValidatorRegistration.Message.Pubkey.String()))
				return
			}
		}

		// Now we have a new registration to process
		numRegNew += 1

		// Save to database
		select {
		case api.validatorRegC <- *signedValidatorRegistration:
		default:
			regLog.Error("validator registration channel full")
		}
	})

	log = log.WithFields(logrus.Fields{
		"timeNeededSec":             time.Since(start).Seconds(),
		"timeNeededMs":              time.Since(start).Milliseconds(),
		"numRegistrations":          numRegTotal,
		"numRegistrationsActive":    numRegActive,
		"numRegistrationsProcessed": numRegProcessed,
		"numRegistrationsNew":       numRegNew,
		"processingStoppedByError":  processingStoppedByError,
	})

	if err != nil {
		handleError(log, http.StatusBadRequest, "error in traversing json")
		return
	}

	log.Info("validator registrations call processed")
	w.WriteHeader(http.StatusOK)
}

func (api *RelayAPI) handleGetHeader(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	slotStr := vars["slot"]
	parentHashHex := vars["parent_hash"]
	proposerPubkeyHex := vars["pubkey"]
	ua := req.UserAgent()
	headSlot := api.headSlot.Load()

	slot, err := strconv.ParseUint(slotStr, 10, 64)
	if err != nil {
		api.RespondError(w, http.StatusBadRequest, common.ErrInvalidSlot.Error())
		return
	}

	requestTime := time.Now().UTC()
	slotStartTimestamp := api.genesisInfo.Data.GenesisTime + (slot * common.SecondsPerSlot)
	msIntoSlot := requestTime.UnixMilli() - int64((slotStartTimestamp * 1000))

	log := api.log.WithFields(logrus.Fields{
		"method":           "getHeader",
		"headSlot":         headSlot,
		"slot":             slotStr,
		"parentHash":       parentHashHex,
		"pubkey":           proposerPubkeyHex,
		"ua":               ua,
		"mevBoostV":        common.GetMevBoostVersionFromUserAgent(ua),
		"requestTimestamp": requestTime.Unix(),
		"slotStartSec":     slotStartTimestamp,
		"msIntoSlot":       msIntoSlot,
	})

	if len(proposerPubkeyHex) != 98 {
		api.RespondError(w, http.StatusBadRequest, common.ErrInvalidPubkey.Error())
		return
	}

	if len(parentHashHex) != 66 {
		api.RespondError(w, http.StatusBadRequest, common.ErrInvalidHash.Error())
		return
	}

	if slot < headSlot {
		api.RespondError(w, http.StatusBadRequest, "slot is too old")
		return
	}

	log.Debug("getHeader request received")

	if slices.Contains(apiNoHeaderUserAgents, ua) {
		log.Info("rejecting getHeader by user agent")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if api.ffForceGetHeader204 {
		log.Info("forced getHeader 204 response")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Only allow requests for the current slot until a certain cutoff time
	if getPayloadRequestCutoffMs > 0 && msIntoSlot > 0 && msIntoSlot > int64(getPayloadRequestCutoffMs) {
		log.Info("getHeader sent too late")
		api.RespondError(w, http.StatusBadRequest, fmt.Sprintf("sent too late - %d ms into slot", msIntoSlot))
		return
	}

	bid, err := api.redis.GetBestBid(slot, parentHashHex, proposerPubkeyHex)
	if err != nil {
		log.WithError(err).Error("could not get bid")
		api.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	if bid.Empty() {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Error on bid without value
	if bid.Value().Cmp(big.NewInt(0)) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	log.WithFields(logrus.Fields{
		"value":     bid.Value().String(),
		"blockHash": bid.BlockHash().String(),
	}).Info("bid delivered")
	api.RespondOK(w, bid)
}

func (api *RelayAPI) handleGetPayload(w http.ResponseWriter, req *http.Request) {
	api.getPayloadCallsInFlight.Add(1)
	defer api.getPayloadCallsInFlight.Done()

	ua := req.UserAgent()
	headSlot := api.headSlot.Load()
	receivedAt := time.Now().UTC()
	log := api.log.WithFields(logrus.Fields{
		"method":                "getPayload",
		"ua":                    ua,
		"mevBoostV":             common.GetMevBoostVersionFromUserAgent(ua),
		"contentLength":         req.ContentLength,
		"headSlot":              headSlot,
		"headSlotEpochPos":      (headSlot % common.SlotsPerEpoch) + 1,
		"idArg":                 req.URL.Query().Get("id"),
		"timestampRequestStart": receivedAt.UnixMilli(),
	})

	// Log at start and end of request
	log.Info("request initiated")
	defer func() {
		log.WithFields(logrus.Fields{
			"timestampRequestFin": time.Now().UTC().UnixMilli(),
			"requestDurationMs":   time.Since(receivedAt).Milliseconds(),
		}).Info("request finished")
	}()

	// Read the body first, so we can decode it later
	body, err := io.ReadAll(req.Body)
	if err != nil {
		if strings.Contains(err.Error(), "i/o timeout") {
			log.WithError(err).Error("getPayload request failed to decode (i/o timeout)")
			api.RespondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		log.WithError(err).Error("could not read body of request from the beacon node")
		api.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Decode payload
	payload := new(common.SignedBlindedBeaconBlock)
	if api.isCapella(headSlot + 1) {
		payload.Capella = new(capella.SignedBlindedBeaconBlock)
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(payload.Capella); err != nil {
			log.WithError(err).Warn("failed to decode capella getPayload request")
			api.RespondError(w, http.StatusBadRequest, "failed to decode capella payload")
			return
		}
	} else {
		payload.Bellatrix = new(boostTypes.SignedBlindedBeaconBlock)
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(payload.Bellatrix); err != nil {
			log.WithError(err).Warn("failed to decode bellatrix getPayload request")
			api.RespondError(w, http.StatusBadRequest, "failed to decode bellatrix payload")
			return
		}
	}

	// Take time after the decoding, and add to logging
	decodeTime := time.Now().UTC()
	slotStartTimestamp := api.genesisInfo.Data.GenesisTime + (payload.Slot() * common.SecondsPerSlot)
	msIntoSlot := decodeTime.UnixMilli() - int64((slotStartTimestamp * 1000))
	log = log.WithFields(logrus.Fields{
		"slot":                 payload.Slot(),
		"slotEpochPos":         (payload.Slot() % common.SlotsPerEpoch) + 1,
		"blockHash":            payload.BlockHash(),
		"slotStartSec":         slotStartTimestamp,
		"msIntoSlot":           msIntoSlot,
		"timestampAfterDecode": decodeTime.UnixMilli(),
		"proposerIndex":        payload.ProposerIndex(),
	})

	// Ensure the proposer index is expected
	api.proposerDutiesLock.RLock()
	slotDuty := api.proposerDutiesMap[payload.Slot()]
	api.proposerDutiesLock.RUnlock()
	if slotDuty == nil {
		log.Warn("could not find slot duty")
	} else {
		log = log.WithField("feeRecipient", slotDuty.Entry.Message.FeeRecipient)
		if slotDuty.ValidatorIndex != payload.ProposerIndex() {
			log.WithField("expectedProposerIndex", slotDuty.ValidatorIndex).Warn("not the expected proposer index")
			api.RespondError(w, http.StatusBadRequest, "not the expected proposer index")
			return
		}
	}

	// Get the proposer pubkey based on the validator index from the payload
	proposerPubkey, found := api.datastore.GetKnownValidatorPubkeyByIndex(payload.ProposerIndex())
	if !found {
		log.Errorf("could not find proposer pubkey for index %d", payload.ProposerIndex())
		api.RespondError(w, http.StatusBadRequest, "could not match proposer index to pubkey")
		return
	}

	// Add proposer pubkey to logs
	log = log.WithField("proposerPubkey", proposerPubkey)

	// Create a BLS pubkey from the hex pubkey
	pk, err := boostTypes.HexToPubkey(proposerPubkey.String())
	if err != nil {
		log.WithError(err).Warn("could not convert pubkey to types.PublicKey")
		api.RespondError(w, http.StatusBadRequest, "could not convert pubkey to types.PublicKey")
		return
	}

	// Validate proposer signature (first attempt verifying the Capella signature)
	if api.isCapella(headSlot + 1) {
		ok, err := boostTypes.VerifySignature(payload.Message(), api.opts.EthNetDetails.DomainBeaconProposerCapella, pk[:], payload.Signature())
		if !ok || err != nil {
			if api.ffLogInvalidSignaturePayload {
				txt, _ := json.Marshal(payload) //nolint:errchkjson
				fmt.Println("payload_invalid_sig_capella: ", string(txt), "pubkey:", proposerPubkey.String())
			}
			log.WithError(err).Warn("could not verify capella payload signature")
			api.RespondError(w, http.StatusBadRequest, "could not verify payload signature")
			return
		}
	} else {
		// Fall-back to verifying the bellatrix signature
		ok, err := boostTypes.VerifySignature(payload.Message(), api.opts.EthNetDetails.DomainBeaconProposerBellatrix, pk[:], payload.Signature())
		if !ok || err != nil {
			if api.ffLogInvalidSignaturePayload {
				txt, _ := json.Marshal(payload) //nolint:errchkjson
				fmt.Println("payload_invalid_sig_bellatrix: ", string(txt), "pubkey:", proposerPubkey.String())
			}
			log.WithError(err).Warn("could not verify bellatrix payload signature")
			api.RespondError(w, http.StatusBadRequest, "could not verify payload signature")
			return
		}
	}

	// Log about received payload (with a valid proposer signature)
	log = log.WithField("timestampAfterSignatureVerify", time.Now().UTC().UnixMilli())
	log.Info("getPayload request received")

	// TODO: store signed blinded block in database (always)

	// Get the response - from Redis, Memcache or DB
	// note that recent mev-boost versions only send getPayload to relays that provided the bid
	getPayloadResp, err := api.datastore.GetGetPayloadResponse(payload.Slot(), proposerPubkey.String(), payload.BlockHash())
	if err != nil || getPayloadResp == nil {
		log.WithError(err).Warn("failed getting execution payload (1/2)")
		time.Sleep(time.Duration(timeoutGetPayloadRetryMs) * time.Millisecond)

		// Try again
		getPayloadResp, err = api.datastore.GetGetPayloadResponse(payload.Slot(), proposerPubkey.String(), payload.BlockHash())
		if err != nil {
			log.WithError(err).Error("failed getting execution payload (2/2) - due to error")
			api.RespondError(w, http.StatusBadRequest, err.Error())
			return
		} else if getPayloadResp == nil {
			log.Warn("failed getting execution payload (2/2)")
			api.RespondError(w, http.StatusBadRequest, "no execution payload for this request")
			return
		}
	}

	// Now we know this relay also has the payload
	log = log.WithField("timestampAfterLoadResponse", time.Now().UTC().UnixMilli())

	// Check whether getPayload has already been called -- TODO: do we need to allow multiple submissions of one blinded block?
	err = api.redis.CheckAndSetLastSlotAndHashDelivered(payload.Slot(), payload.BlockHash())
	log = log.WithField("timestampAfterAlreadyDeliveredCheck", time.Now().UTC().UnixMilli())
	if err != nil {
		if errors.Is(err, datastore.ErrAnotherPayloadAlreadyDeliveredForSlot) {
			// BAD VALIDATOR, 2x GETPAYLOAD FOR DIFFERENT PAYLOADS
			log.Warn("validator called getPayload twice for different payload hashes")
			api.RespondError(w, http.StatusBadRequest, "another payload for this slot was already delivered")
			return
		} else if errors.Is(err, datastore.ErrPastSlotAlreadyDelivered) {
			// BAD VALIDATOR, 2x GETPAYLOAD FOR PAST SLOT
			log.Warn("validator called getPayload for past slot")
			api.RespondError(w, http.StatusBadRequest, "payload for this slot was already delivered")
		} else if errors.Is(err, redis.TxFailedErr) {
			// BAD VALIDATOR, 2x GETPAYLOAD + RACE
			log.Warn("validator called getPayload twice (race)")
			api.RespondError(w, http.StatusBadRequest, "payload for this slot was already delivered (race)")
			return
		}
		log.WithError(err).Error("redis.CheckAndSetLastSlotAndHashDelivered failed")
	}

	// Handle early/late requests
	if msIntoSlot < 0 {
		// Wait until slot start (t=0) if still in the future
		_msSinceSlotStart := time.Now().UTC().UnixMilli() - int64((slotStartTimestamp * 1000))
		if _msSinceSlotStart < 0 {
			delayMillis := _msSinceSlotStart * -1
			log = log.WithField("delayMillis", delayMillis)
			log.Info("waiting until slot start t=0")
			time.Sleep(time.Duration(delayMillis) * time.Millisecond)
		}
	} else if getPayloadRequestCutoffMs > 0 && msIntoSlot > int64(getPayloadRequestCutoffMs) {
		// Reject requests after cutoff time
		log.Warn("getPayload sent too late")
		api.RespondError(w, http.StatusBadRequest, fmt.Sprintf("sent too late - %d ms into slot", msIntoSlot))

		go func() {
			err := api.db.InsertTooLateGetPayload(payload.Slot(), proposerPubkey.String(), payload.BlockHash(), slotStartTimestamp, uint64(receivedAt.UnixMilli()), uint64(decodeTime.UnixMilli()), uint64(msIntoSlot))
			if err != nil {
				log.WithError(err).Error("failed to insert payload too late into db")
			}
		}()
		return
	}

	// Check that ExecutionPayloadHeader fields (sent by the proposer) match our known ExecutionPayload
	err = EqExecutionPayloadToHeader(payload, getPayloadResp)
	if err != nil {
		log.WithError(err).Warn("ExecutionPayloadHeader not matching known ExecutionPayload")
		api.RespondError(w, http.StatusBadRequest, "invalid execution payload header")
		return
	}

	// Publish the signed beacon block via beacon-node
	timeBeforePublish := time.Now().UTC().UnixMilli()
	log = log.WithField("timestampBeforePublishing", timeBeforePublish)
	signedBeaconBlock := common.SignedBlindedBeaconBlockToBeaconBlock(payload, getPayloadResp)
	code, err := api.beaconClient.PublishBlock(signedBeaconBlock) // errors are logged inside
	if err != nil || code != http.StatusOK {
		log.WithError(err).WithField("code", code).Error("failed to publish block")
		api.RespondError(w, http.StatusBadRequest, "failed to publish block")
		return
	}
	timeAfterPublish := time.Now().UTC().UnixMilli()
	msNeededForPublishing := uint64(timeAfterPublish - timeBeforePublish)
	log = log.WithField("timestampAfterPublishing", timeAfterPublish)
	log.WithField("msNeededForPublishing", msNeededForPublishing).Info("block published through beacon node")

	// give the beacon network some time to propagate the block
	time.Sleep(time.Duration(getPayloadResponseDelayMs) * time.Millisecond)

	// respond to the HTTP request
	api.RespondOK(w, getPayloadResp)
	log = log.WithFields(logrus.Fields{
		"numTx":       getPayloadResp.NumTx(),
		"blockNumber": payload.BlockNumber(),
	})
	log.Info("execution payload delivered")

	// Save information about delivered payload
	go func() {
		bidTrace, err := api.redis.GetBidTrace(payload.Slot(), proposerPubkey.String(), payload.BlockHash())
		if err != nil {
			log.WithError(err).Error("failed to get bidTrace for delivered payload from redis")
		}

		err = api.db.SaveDeliveredPayload(bidTrace, payload, decodeTime, msNeededForPublishing)
		if err != nil {
			log.WithError(err).WithFields(logrus.Fields{
				"bidTrace": bidTrace,
				"payload":  payload,
			}).Error("failed to save delivered payload")
		}

		// Increment builder stats
		err = api.db.IncBlockBuilderStatsAfterGetPayload(bidTrace.BuilderPubkey.String())
		if err != nil {
			log.WithError(err).Error("failed to increment builder-stats after getPayload")
		}

		// Wait until optimistic blocks are complete.
		api.optimisticBlocksWG.Wait()

		// Check if there is a demotion for the winning block.
		_, err = api.db.GetBuilderDemotion(bidTrace)
		// If demotion not found, we are done!
		if errors.Is(err, sql.ErrNoRows) {
			log.Info("no demotion in getPayload, successful block proposal")
			return
		}
		if err != nil {
			log.WithError(err).Error("failed to read demotion table in getPayload")
			return
		}
		// Demotion found, update the demotion table with refund data.
		builderPubkey := bidTrace.BuilderPubkey.String()
		log = log.WithFields(logrus.Fields{
			"builderPubkey": builderPubkey,
			"slot":          bidTrace.Slot,
			"blockHash":     bidTrace.BlockHash,
		})
		log.Warn("demotion found in getPayload, inserting refund justification")

		// Prepare refund data.
		signedBeaconBlock := common.SignedBlindedBeaconBlockToBeaconBlock(payload, getPayloadResp)

		// Get registration entry from the DB.
		registrationEntry, err := api.db.GetValidatorRegistration(proposerPubkey.String())
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				log.WithError(err).Error("no registration found for validator " + proposerPubkey.String())
			} else {
				log.WithError(err).Error("error reading validator registration")
			}
		}
		var signedRegistration *boostTypes.SignedValidatorRegistration
		if registrationEntry != nil {
			signedRegistration, err = registrationEntry.ToSignedValidatorRegistration()
			if err != nil {
				log.WithError(err).Error("error converting registration to signed registration")
			}
		}

		err = api.db.UpdateBuilderDemotion(bidTrace, signedBeaconBlock, signedRegistration)
		if err != nil {
			log.WithFields(logrus.Fields{
				"errorWritingRefundToDB": true,
				"bidTrace":               bidTrace,
				"signedBeaconBlock":      signedBeaconBlock,
				"signedRegistration":     signedRegistration,
			}).WithError(err).Error("unable to update builder demotion with refund justification")
		}
	}()
}

// --------------------
//
//	BLOCK BUILDER APIS
//
// --------------------
func (api *RelayAPI) handleBuilderGetValidators(w http.ResponseWriter, req *http.Request) {
	api.proposerDutiesLock.RLock()
	resp := api.proposerDutiesResponse
	api.proposerDutiesLock.RUnlock()
	_, err := w.Write(*resp)
	if err != nil {
		api.log.WithError(err).Warn("failed to write response for builderGetValidators")
	}
}

func (api *RelayAPI) handleSubmitNewBlock(w http.ResponseWriter, req *http.Request) { //nolint:gocognit,maintidx
	var pf common.Profile
	var prevTime, nextTime time.Time

	headSlot := api.headSlot.Load()
	receivedAt := time.Now().UTC()
	prevTime = receivedAt

	args := req.URL.Query()
	isCancellationEnabled := args.Get("cancellations") == "1"

	log := api.log.WithFields(logrus.Fields{
		"method":                "submitNewBlock",
		"contentLength":         req.ContentLength,
		"headSlot":              headSlot,
		"cancellationEnabled":   isCancellationEnabled,
		"timestampRequestStart": receivedAt.UnixMilli(),
	})

	// Log at start and end of request
	log.Info("request initiated")
	defer func() {
		log.WithFields(logrus.Fields{
			"timestampRequestFin": time.Now().UTC().UnixMilli(),
			"requestDurationMs":   time.Since(receivedAt).Milliseconds(),
		}).Info("request finished")
	}()

	// If cancellations are disabled but builder requested it, return error
	if isCancellationEnabled && !api.ffEnableCancellations {
		log.Info("builder submitted with cancellations enabled, but feature flag is disabled")
		api.RespondError(w, http.StatusBadRequest, "cancellations are disabled")
		return
	}

	var err error
	var r io.Reader = req.Body
	isGzip := req.Header.Get("Content-Encoding") == "gzip"
	log = log.WithField("reqIsGzip", isGzip)
	if isGzip {
		r, err = gzip.NewReader(req.Body)
		if err != nil {
			log.WithError(err).Warn("could not create gzip reader")
			api.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	limitReader := io.LimitReader(r, 10*1024*1024) // 10 MB
	requestPayloadBytes, err := io.ReadAll(limitReader)
	if err != nil {
		log.WithError(err).Warn("could not read payload")
		api.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	payload := new(common.BuilderSubmitBlockRequest)

	// Check for SSZ encoding
	contentType := req.Header.Get("Content-Type")
	if contentType == "application/octet-stream" {
		log = log.WithField("reqContentType", "ssz")
		payload.Capella = new(builderCapella.SubmitBlockRequest)
		if err = payload.Capella.UnmarshalSSZ(requestPayloadBytes); err != nil {
			log.WithError(err).Warn("could not decode payload - SSZ")

			// SSZ decoding failed. try JSON as fallback (some builders used octet-stream for json before)
			if err2 := json.Unmarshal(requestPayloadBytes, payload); err2 != nil {
				log.WithError(fmt.Errorf("%w / %w", err, err2)).Warn("could not decode payload - SSZ or JSON")
				api.RespondError(w, http.StatusBadRequest, err.Error())
				return
			}
			log = log.WithField("reqContentType", "json")
		} else {
			log.Debug("received ssz-encoded payload")
		}
	} else {
		log = log.WithField("reqContentType", "json")
		if err := json.Unmarshal(requestPayloadBytes, payload); err != nil {
			log.WithError(err).Warn("could not decode payload - JSON")
			api.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	nextTime = time.Now().UTC()
	pf.Decode = uint64(nextTime.Sub(prevTime).Microseconds())
	prevTime = nextTime

	log = log.WithFields(logrus.Fields{
		"timestampAfterDecoding": time.Now().UTC().UnixMilli(),
		"slot":                   payload.Slot(),
		"builderPubkey":          payload.BuilderPubkey().String(),
		"blockHash":              payload.BlockHash(),
		"proposerPubkey":         payload.ProposerPubkey(),
		"parentHash":             payload.ParentHash(),
		"value":                  payload.Value().String(),
		"numTx":                  payload.NumTx(),
	})

	if payload.Message() == nil || !payload.HasExecutionPayload() {
		api.RespondError(w, http.StatusBadRequest, "missing parts of the payload")
		return
	}

	if api.isCapella(headSlot+1) && payload.Capella == nil {
		log.Info("rejecting submission - non capella payload for capella fork")
		api.RespondError(w, http.StatusBadRequest, "not capella payload")
		return
	} else if api.isBellatrix(headSlot+1) && payload.Bellatrix == nil {
		log.Info("rejecting submission - non bellatrix payload for bellatrix fork")
		api.RespondError(w, http.StatusBadRequest, "not belltrix payload")
		return
	}

	if payload.Slot() <= headSlot {
		log.Info("submitNewBlock failed: submission for past slot")
		api.RespondError(w, http.StatusBadRequest, "submission for past slot")
		return
	}

	builderPubkey := payload.BuilderPubkey()
	builderEntry, ok := api.blockBuildersCache[builderPubkey.String()]
	if !ok {
		log.Warnf("unable to read builder: %s from the builder cache, using low-prio and no collateral", builderPubkey.String())
		builderEntry = &blockBuilderCacheEntry{
			status: common.BuilderStatus{
				IsHighPrio:    false,
				IsOptimistic:  false,
				IsBlacklisted: false,
			},
			collateral: big.NewInt(0),
		}
	}
	log = log.WithFields(logrus.Fields{
		"builderEntry":      builderEntry,
		"builderIsHighPrio": builderEntry.status.IsHighPrio,
	})

	// Timestamp check
	expectedTimestamp := api.genesisInfo.Data.GenesisTime + (payload.Slot() * common.SecondsPerSlot)
	if payload.Timestamp() != expectedTimestamp {
		log.Warnf("incorrect timestamp. got %d, expected %d", payload.Timestamp(), expectedTimestamp)
		api.RespondError(w, http.StatusBadRequest, fmt.Sprintf("incorrect timestamp. got %d, expected %d", payload.Timestamp(), expectedTimestamp))
		return
	}

	if builderEntry.status.IsBlacklisted {
		log.Info("builder is blacklisted")
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		return
	}

	// In case only high-prio requests are accepted, fail others
	if api.ffDisableLowPrioBuilders && !builderEntry.status.IsHighPrio {
		log.Info("rejecting low-prio builder (ff-disable-low-prio-builders)")
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		return
	}

	log = log.WithField("timestampAfterChecks1", time.Now().UTC().UnixMilli())

	// ensure correct feeRecipient is used
	api.proposerDutiesLock.RLock()
	slotDuty := api.proposerDutiesMap[payload.Slot()]
	api.proposerDutiesLock.RUnlock()
	if slotDuty == nil {
		log.Warn("could not find slot duty")
		api.RespondError(w, http.StatusBadRequest, "could not find slot duty")
		return
	} else if !strings.EqualFold(slotDuty.Entry.Message.FeeRecipient.String(), payload.ProposerFeeRecipient()) {
		log.WithFields(logrus.Fields{
			"expectedFeeRecipient": slotDuty.Entry.Message.FeeRecipient.String(),
			"actualFeeRecipient":   payload.ProposerFeeRecipient(),
		}).Info("fee recipient does not match")
		api.RespondError(w, http.StatusBadRequest, "fee recipient does not match")
		return
	}

	// Don't accept blocks with 0 value
	if payload.Value().Cmp(ZeroU256.BigInt()) == 0 || payload.NumTx() == 0 {
		log.Info("submitNewBlock failed: block with 0 value or no txs")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Sanity check the submission
	err = SanityCheckBuilderBlockSubmission(payload)
	if err != nil {
		log.WithError(err).Info("block submission sanity checks failed")
		api.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	log = log.WithField("timestampBeforeAttributesCheck", time.Now().UTC().UnixMilli())

	api.payloadAttributesLock.RLock()
	attrs, ok := api.payloadAttributes[payload.ParentHash()]
	api.payloadAttributesLock.RUnlock()
	if !ok || payload.Slot() != attrs.slot {
		log.Warn("payload attributes not (yet) known")
		api.RespondError(w, http.StatusBadRequest, "payload attributes not (yet) known")
		return
	}

	if payload.Random() != attrs.payloadAttributes.PrevRandao {
		msg := fmt.Sprintf("incorrect prev_randao - got: %s, expected: %s", payload.Random(), attrs.payloadAttributes.PrevRandao)
		log.Info(msg)
		api.RespondError(w, http.StatusBadRequest, msg)
		return
	}

	if api.isCapella(payload.Slot()) { // Capella requires correct withdrawals
		withdrawalsRoot, err := ComputeWithdrawalsRoot(payload.Withdrawals())
		if err != nil {
			log.WithError(err).Warn("could not compute withdrawals root from payload")
			api.RespondError(w, http.StatusBadRequest, "could not compute withdrawals root")
			return
		}

		if withdrawalsRoot != attrs.withdrawalsRoot {
			msg := fmt.Sprintf("incorrect withdrawals root - got: %s, expected: %s", withdrawalsRoot.String(), attrs.withdrawalsRoot.String())
			log.Info(msg)
			api.RespondError(w, http.StatusBadRequest, msg)
			return
		}
	}

	// Verify the signature
	log = log.WithField("timestampBeforeSignatureCheck", time.Now().UTC().UnixMilli())
	signature := payload.Signature()
	ok, err = boostTypes.VerifySignature(payload.Message(), api.opts.EthNetDetails.DomainBuilder, builderPubkey[:], signature[:])
	log = log.WithField("timestampAfterSignatureCheck", time.Now().UTC().UnixMilli())
	if !ok || err != nil {
		log.WithError(err).Warn("could not verify builder signature")
		api.RespondError(w, http.StatusBadRequest, "invalid signature")
		return
	}

	// Reject new submissions once the payload for this slot was delivered - TODO: store in memory as well
	slotLastPayloadDelivered, err := api.redis.GetLastSlotDelivered()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.WithError(err).Error("failed to get delivered payload slot from redis")
	} else if payload.Slot() <= slotLastPayloadDelivered {
		log.Info("rejecting submission because payload for this slot was already delivered")
		api.RespondError(w, http.StatusBadRequest, "payload for this slot was already delivered")
		return
	}

	var wasSimulated, optimisticSubmission bool
	var requestErr, validationErr error
	var eligibleAt time.Time

	// Save the builder submission to the database whenever this function ends
	defer func() {
		savePayloadToDatabase := !api.ffDisablePayloadDBStorage
		submissionEntry, err := api.db.SaveBuilderBlockSubmission(payload, requestErr, validationErr, receivedAt, eligibleAt, wasSimulated, savePayloadToDatabase, pf, optimisticSubmission)
		if err != nil {
			log.WithError(err).WithField("payload", payload).Error("saving builder block submission to database failed")
			return
		}

		err = api.db.UpsertBlockBuilderEntryAfterSubmission(submissionEntry, validationErr != nil)
		if err != nil {
			log.WithError(err).Error("failed to upsert block-builder-entry")
		}
	}()

	// Grab floor bid value
	floorBidValue, err := api.redis.GetFloorBidValue(payload.Slot(), payload.ParentHash(), payload.ProposerPubkey())
	if err != nil {
		log.WithError(err).Error("failed to get floor bid value from redis")
	} else {
		isBidAboveFloor := payload.Value().Cmp(floorBidValue) == 1
		log = log.WithFields(logrus.Fields{
			"floorBidValue":   floorBidValue.String(),
			"isBidAboveFloor": isBidAboveFloor,
		})

		// Without cancellations, discard bids below floor value
		if !isCancellationEnabled && !isBidAboveFloor {
			log.Info("ignoring submission without cancellation and below floor bid value")
			api.RespondMsg(w, http.StatusAccepted, "ignoring submission without cancellation and below floor bid value")
			return
		}
	}

	// Get the latest top bid value from Redis
	bidIsTopBid := false
	topBidValue, err := api.redis.GetTopBidValue(payload.Slot(), payload.ParentHash(), payload.ProposerPubkey())
	if err != nil {
		log.WithError(err).Error("failed to get top bid value from redis")
	} else {
		bidIsTopBid = payload.Value().Cmp(topBidValue) == 1
		log = log.WithFields(logrus.Fields{
			"topBidValue":    topBidValue.String(),
			"newBidIsTopBid": bidIsTopBid,
		})
	}

	// Simulate the block submission and save to db
	fastTrackValidation := builderEntry.status.IsHighPrio && bidIsTopBid
	timeBeforeValidation := time.Now().UTC()

	log = log.WithFields(logrus.Fields{
		"timestampBeforeValidation": timeBeforeValidation.UTC().UnixMilli(),
		"fastTrackValidation":       fastTrackValidation,
	})

	nextTime = time.Now().UTC()
	pf.Prechecks = uint64(nextTime.Sub(prevTime).Microseconds())
	prevTime = nextTime

	// Construct simulation request.
	opts := blockSimOptions{
		isHighPrio: builderEntry.status.IsHighPrio,
		fastTrack:  fastTrackValidation,
		log:        log,
		builder:    builderEntry,
		req: &common.BuilderBlockValidationRequest{
			BuilderSubmitBlockRequest: *payload,
			RegisteredGasLimit:        slotDuty.Entry.Message.GasLimit,
		},
	}
	// With sufficient collateral, process the block optimistically.
	if builderEntry.status.IsOptimistic &&
		builderEntry.collateral.Cmp(payload.Value()) >= 0 &&
		payload.Slot() == api.optimisticSlot.Load() {
		optimisticSubmission = true
		go api.processOptimisticBlock(opts)
	} else {
		// Simulate block (synchronously)
		reqErr, simErr := api.simulateBlock(req.Context(), opts) // success/error logging happens inside
		validationDurationMs := time.Since(timeBeforeValidation).Milliseconds()
		log = log.WithFields(logrus.Fields{
			"timestampAfterValidation": time.Now().UTC().UnixMilli(),
			"validationDurationMs":     validationDurationMs,
		})
		if reqErr != nil { // Request error
			if os.IsTimeout(reqErr) {
				api.RespondError(w, http.StatusGatewayTimeout, "validation request timeout")
			} else {
				api.RespondError(w, http.StatusBadRequest, reqErr.Error())
			}
			return
		} else {
			wasSimulated = true
			if simErr != nil {
				api.RespondError(w, http.StatusBadRequest, simErr.Error())
				return
			}
		}
	}

	nextTime = time.Now().UTC()
	pf.Simulation = uint64(nextTime.Sub(prevTime).Microseconds())
	prevTime = nextTime

	// If cancellations are enabled, then abort now if this submission is not the latest one
	if isCancellationEnabled {
		// Ensure this request is still the latest one. This logic intentionally ignores the value of the bids and makes the current active bid the one
		// that arrived at the relay last. This allows for builders to reduce the value of their bid (effectively cancel a high bid) by ensuring a lower
		// bid arrives later. Even if the higher bid takes longer to simulate, by checking the receivedAt timestamp, this logic ensures that the low bid
		// is not overwritten by the high bid.
		//
		// NOTE: this can lead to a rather tricky race condition. If a builder submits two blocks to the relay concurrently, then the randomness of network
		// latency will make it impossible to predict which arrives first. Thus a high bid could unintentionally be overwritten by a low bid that happened
		// to arrive a few microseconds later. If builders are submitting blocks at a frequency where they cannot reliably predict which bid will arrive at
		// the relay first, they should instead use multiple pubkeys to avoid uninitentionally overwriting their own bids.
		latestPayloadReceivedAt, err := api.redis.GetBuilderLatestPayloadReceivedAt(payload.Slot(), payload.BuilderPubkey().String(), payload.ParentHash(), payload.ProposerPubkey())
		if err != nil {
			log.WithError(err).Error("failed getting latest payload receivedAt from redis")
		} else if receivedAt.UnixMilli() < latestPayloadReceivedAt {
			log.Infof("already have a newer payload: now=%d / prev=%d", receivedAt.UnixMilli(), latestPayloadReceivedAt)
			api.RespondError(w, http.StatusBadRequest, "already using a newer payload")
			return
		}
	}

	// Prepare the response data
	getHeaderResponse, err := common.BuildGetHeaderResponse(payload, api.blsSk, api.publicKey, api.opts.EthNetDetails.DomainBuilder)
	if err != nil {
		log.WithError(err).Error("could not sign builder bid")
		api.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	getPayloadResponse, err := common.BuildGetPayloadResponse(payload)
	if err != nil {
		log.WithError(err).Error("could not build getPayload response")
		api.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	bidTrace := common.BidTraceV2{
		BidTrace:    *payload.Message(),
		BlockNumber: payload.BlockNumber(),
		NumTx:       uint64(payload.NumTx()),
	}

	//
	// Save to Redis
	//
	// 1. Save BidTrace
	log = log.WithField("timestampBeforeUpdateTopBid", time.Now().UTC().UnixMilli())
	err = api.redis.SaveBidTrace(&bidTrace)
	if err != nil {
		log.WithError(err).Error("failed saving bidTrace in redis")
		api.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 2. Save bid and recalculate top bid
	updateBidResult, err := api.redis.SaveBidAndUpdateTopBid(payload, getPayloadResponse, getHeaderResponse, receivedAt, isCancellationEnabled, floorBidValue)
	if err != nil {
		log.WithError(err).Error("could not save bid and update top bids")
		api.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Add fields to logs
	log = log.WithFields(logrus.Fields{
		"wasBidSavedInRedis":      updateBidResult.WasBidSaved,
		"wasTopBidUpdated":        updateBidResult.WasTopBidUpdated,
		"topBidValue":             updateBidResult.TopBidValue,
		"prevTopBidValue":         updateBidResult.PrevTopBidValue,
		"timestampAfterBidUpdate": time.Now().UTC().UnixMilli(),
	})

	if updateBidResult.WasBidSaved {
		// Bid is eligible to win the auction
		eligibleAt = time.Now().UTC()
		log = log.WithField("timestampEligibleAt", eligibleAt.UnixMilli())

		// Save to memcache in the background
		if api.memcached != nil {
			go func() {
				err = api.memcached.SaveExecutionPayload(payload.Slot(), payload.ProposerPubkey(), payload.BlockHash(), getPayloadResponse)
				if err != nil {
					log.WithError(err).Error("failed saving execution payload in memcached")
				}
			}()
		}
	}

	nextTime = time.Now().UTC()
	pf.RedisUpdate = uint64(nextTime.Sub(prevTime).Microseconds())
	pf.Total = uint64(nextTime.Sub(receivedAt).Microseconds())

	// Respond with OK (TODO: proper response data type https://flashbots.notion.site/Relay-API-Spec-5fb0819366954962bc02e81cb33840f5#fa719683d4ae4a57bc3bf60e138b0dc6)
	// All done
	log.Info("received block from builder")
	w.WriteHeader(http.StatusOK)
}

// ---------------
//
//	INTERNAL APIS
//
// ---------------
func (api *RelayAPI) handleInternalBuilderStatus(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	builderPubkey := vars["pubkey"]
	builderEntry, err := api.db.GetBlockBuilderByPubkey(builderPubkey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			api.RespondError(w, http.StatusBadRequest, "builder not found")
			return
		}

		api.log.WithError(err).Error("could not get block builder")
		api.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Method == http.MethodGet {
		api.RespondOK(w, builderEntry)
		return
	} else if req.Method == http.MethodPost || req.Method == http.MethodPut || req.Method == http.MethodPatch {
		st := common.BuilderStatus{
			IsHighPrio:    builderEntry.IsHighPrio,
			IsBlacklisted: builderEntry.IsBlacklisted,
			IsOptimistic:  builderEntry.IsOptimistic,
		}
		trueStr := "true"
		args := req.URL.Query()
		if args.Get("high_prio") != "" {
			st.IsHighPrio = args.Get("high_prio") == trueStr
		}
		if args.Get("blacklisted") != "" {
			st.IsBlacklisted = args.Get("blacklisted") == trueStr
		}
		if args.Get("optimistic") != "" {
			st.IsOptimistic = args.Get("optimistic") == trueStr
		}
		api.log.WithFields(logrus.Fields{
			"builderPubkey": builderPubkey,
			"isHighPrio":    st.IsHighPrio,
			"isBlacklisted": st.IsBlacklisted,
			"isOptimistic":  st.IsOptimistic,
		}).Info("updating builder status")
		err := api.db.SetBlockBuilderStatus(builderPubkey, st)
		if err != nil {
			err := fmt.Errorf("error setting builder: %v status: %w", builderPubkey, err)
			api.log.Error(err)
			api.RespondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		api.RespondOK(w, st)
	}
}

func (api *RelayAPI) handleInternalBuilderCollateral(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	builderPubkey := vars["pubkey"]
	if req.Method == http.MethodPost || req.Method == http.MethodPut {
		args := req.URL.Query()
		collateral := args.Get("collateral")
		value := args.Get("value")
		log := api.log.WithFields(logrus.Fields{
			"pubkey":     builderPubkey,
			"collateral": collateral,
			"value":      value,
		})
		log.Infof("updating builder collateral")
		if err := api.db.SetBlockBuilderCollateral(builderPubkey, collateral, value); err != nil {
			fullErr := fmt.Errorf("unable to set collateral in db for pubkey: %v: %w", builderPubkey, err)
			log.Error(fullErr.Error())
			api.RespondError(w, http.StatusInternalServerError, fullErr.Error())
			return
		}
		api.RespondOK(w, NilResponse)
	}
}

// -----------
//  DATA APIS
// -----------

func (api *RelayAPI) handleDataProposerPayloadDelivered(w http.ResponseWriter, req *http.Request) {
	var err error
	args := req.URL.Query()

	filters := database.GetPayloadsFilters{
		Limit: 200,
	}

	if args.Get("slot") != "" && args.Get("cursor") != "" {
		api.RespondError(w, http.StatusBadRequest, "cannot specify both slot and cursor")
		return
	} else if args.Get("slot") != "" {
		filters.Slot, err = strconv.ParseUint(args.Get("slot"), 10, 64)
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid slot argument")
			return
		}
	} else if args.Get("cursor") != "" {
		filters.Cursor, err = strconv.ParseUint(args.Get("cursor"), 10, 64)
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid cursor argument")
			return
		}
	}

	if args.Get("block_hash") != "" {
		var hash boostTypes.Hash
		err = hash.UnmarshalText([]byte(args.Get("block_hash")))
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid block_hash argument")
			return
		}
		filters.BlockHash = args.Get("block_hash")
	}

	if args.Get("block_number") != "" {
		filters.BlockNumber, err = strconv.ParseUint(args.Get("block_number"), 10, 64)
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid block_number argument")
			return
		}
	}

	if args.Get("proposer_pubkey") != "" {
		if err = checkBLSPublicKeyHex(args.Get("proposer_pubkey")); err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid proposer_pubkey argument")
			return
		}
		filters.ProposerPubkey = args.Get("proposer_pubkey")
	}

	if args.Get("builder_pubkey") != "" {
		if err = checkBLSPublicKeyHex(args.Get("builder_pubkey")); err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid builder_pubkey argument")
			return
		}
		filters.BuilderPubkey = args.Get("builder_pubkey")
	}

	if args.Get("limit") != "" {
		_limit, err := strconv.ParseUint(args.Get("limit"), 10, 64)
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid limit argument")
			return
		}
		if _limit > filters.Limit {
			api.RespondError(w, http.StatusBadRequest, fmt.Sprintf("maximum limit is %d", filters.Limit))
			return
		}
		filters.Limit = _limit
	}

	if args.Get("order_by") == "value" {
		filters.OrderByValue = 1
	} else if args.Get("order_by") == "-value" {
		filters.OrderByValue = -1
	}

	deliveredPayloads, err := api.db.GetRecentDeliveredPayloads(filters)
	if err != nil {
		api.log.WithError(err).Error("error getting recent payloads")
		api.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	response := make([]common.BidTraceV2JSON, len(deliveredPayloads))
	for i, payload := range deliveredPayloads {
		response[i] = database.DeliveredPayloadEntryToBidTraceV2JSON(payload)
	}

	api.RespondOK(w, response)
}

func (api *RelayAPI) handleDataBuilderBidsReceived(w http.ResponseWriter, req *http.Request) {
	var err error
	args := req.URL.Query()

	filters := database.GetBuilderSubmissionsFilters{
		Limit:         500,
		Slot:          0,
		BlockHash:     "",
		BlockNumber:   0,
		BuilderPubkey: "",
	}

	if args.Get("cursor") != "" {
		api.RespondError(w, http.StatusBadRequest, "cursor argument not supported")
		return
	}

	if args.Get("slot") != "" {
		filters.Slot, err = strconv.ParseUint(args.Get("slot"), 10, 64)
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid slot argument")
			return
		}
	}

	if args.Get("block_hash") != "" {
		var hash boostTypes.Hash
		err = hash.UnmarshalText([]byte(args.Get("block_hash")))
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid block_hash argument")
			return
		}
		filters.BlockHash = args.Get("block_hash")
	}

	if args.Get("block_number") != "" {
		filters.BlockNumber, err = strconv.ParseUint(args.Get("block_number"), 10, 64)
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid block_number argument")
			return
		}
	}

	if args.Get("builder_pubkey") != "" {
		if err = checkBLSPublicKeyHex(args.Get("builder_pubkey")); err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid builder_pubkey argument")
			return
		}
		filters.BuilderPubkey = args.Get("builder_pubkey")
	}

	// at least one query arguments is required
	if filters.Slot == 0 && filters.BlockHash == "" && filters.BlockNumber == 0 && filters.BuilderPubkey == "" {
		api.RespondError(w, http.StatusBadRequest, "need to query for specific slot or block_hash or block_number or builder_pubkey")
		return
	}

	if args.Get("limit") != "" {
		_limit, err := strconv.ParseUint(args.Get("limit"), 10, 64)
		if err != nil {
			api.RespondError(w, http.StatusBadRequest, "invalid limit argument")
			return
		}
		if _limit > filters.Limit {
			api.RespondError(w, http.StatusBadRequest, fmt.Sprintf("maximum limit is %d", filters.Limit))
			return
		}
		filters.Limit = _limit
	}

	blockSubmissions, err := api.db.GetBuilderSubmissions(filters)
	if err != nil {
		api.log.WithError(err).Error("error getting recent payloads")
		api.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	response := make([]common.BidTraceV2WithTimestampJSON, len(blockSubmissions))
	for i, payload := range blockSubmissions {
		response[i] = database.BuilderSubmissionEntryToBidTraceV2WithTimestampJSON(payload)
	}

	api.RespondOK(w, response)
}

func (api *RelayAPI) handleDataValidatorRegistration(w http.ResponseWriter, req *http.Request) {
	pkStr := req.URL.Query().Get("pubkey")
	if pkStr == "" {
		api.RespondError(w, http.StatusBadRequest, "missing pubkey argument")
		return
	}

	var pk boostTypes.PublicKey
	err := pk.UnmarshalText([]byte(pkStr))
	if err != nil {
		api.RespondError(w, http.StatusBadRequest, "invalid pubkey")
		return
	}

	registrationEntry, err := api.db.GetValidatorRegistration(pkStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			api.RespondError(w, http.StatusBadRequest, "no registration found for validator "+pkStr)
			return
		}
		api.log.WithError(err).Error("error getting validator registration")
		api.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	signedRegistration, err := registrationEntry.ToSignedValidatorRegistration()
	if err != nil {
		api.log.WithError(err).Error("error converting registration entry to signed validator registration")
		api.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	api.RespondOK(w, signedRegistration)
}
