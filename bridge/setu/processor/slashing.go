package processor

import (
	"bytes"
	"encoding/hex"
	"encoding/json"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/maticnetwork/bor/accounts/abi"
	"github.com/maticnetwork/bor/core/types"
	"github.com/maticnetwork/heimdall/bridge/setu/util"
	"github.com/maticnetwork/heimdall/contracts/stakinginfo"
	"github.com/maticnetwork/heimdall/helper"
	slashingTypes "github.com/maticnetwork/heimdall/slashing/types"
	hmTypes "github.com/maticnetwork/heimdall/types"
)

// SlashingProcessor - process slashing related events
type SlashingProcessor struct {
	BaseProcessor
	stakingInfoAbi *abi.ABI
}

// NewSlashingProcessor - add  abi to slashing processor
func NewSlashingProcessor(stakingInfoAbi *abi.ABI) *SlashingProcessor {
	slashingProcessor := &SlashingProcessor{
		stakingInfoAbi: stakingInfoAbi,
	}
	return slashingProcessor
}

// Start starts new block subscription
func (sp *SlashingProcessor) Start() error {
	sp.Logger.Info("Starting")
	// TODO - slashing. remove this. just for testing
	// sp.sendTickToHeimdall()
	return nil
}

// RegisterTasks - Registers slashing related tasks with machinery
func (sp *SlashingProcessor) RegisterTasks() {
	sp.Logger.Info("Registering slashing related tasks")
	sp.queueConnector.Server.RegisterTask("sendTickToHeimdall", sp.sendTickToHeimdall)
	sp.queueConnector.Server.RegisterTask("sendTickToRootchain", sp.sendTickToRootchain)
	sp.queueConnector.Server.RegisterTask("sendTickAckToHeimdall", sp.sendTickAckToHeimdall)

}

// processSlashLimitEvent - processes slash limit event
func (sp *SlashingProcessor) sendTickToHeimdall(eventBytes string, txHeight int64, txHash string) (err error) {
	sp.Logger.Info("Recevied sendTickToHeimdall request", "eventBytes", eventBytes, "txHeight", txHeight, "txHash", txHash)
	var event = sdk.StringEvent{}
	if err := json.Unmarshal([]byte(eventBytes), &event); err != nil {
		sp.Logger.Error("Error unmarshalling event from heimdall", "error", err)
		return err
	}

	latestSlashInfoHash := hmTypes.ZeroHeimdallHash
	//Get DividendAccountRoot from HeimdallServer
	if latestSlashInfoHash, err = sp.fetchLatestSlashInfoHash(); err != nil {
		sp.Logger.Info("Error while fetching latest slashinfo hash from HeimdallServer", "err", err)
		return err
	}

	sp.Logger.Info("✅ Creating and broadcasting Tick tx",
		"From", hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
		"slashingInfoHash", latestSlashInfoHash,
	)

	// create msg Tick message
	msg := slashingTypes.NewMsgtick(
		hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
		latestSlashInfoHash,
	)

	// return broadcast to heimdall
	if err := sp.txBroadcaster.BroadcastToHeimdall(msg); err != nil {
		sp.Logger.Error("Error while broadcasting Tick msg to heimdall", "error", err)
		return err
	}
	return nil
}

/*
	sendTickToRootchain - create and submit tick tx to rootchain to slashing faulty validators
	1. Fetch sigs from heimdall using txHash
	2. Fetch slashing info from heimdall via Rest call
	3. Verify if this tick tx is already submitted to rootchain using nonce data
	4. create tick tx and submit to rootchain
*/
func (sp *SlashingProcessor) sendTickToRootchain(eventBytes string, txHeight int64, txHash string) error {
	sp.Logger.Info("Recevied sendTickToRootchain request", "eventBytes", eventBytes, "txHeight", txHeight, "txHash", txHash)
	var event = sdk.StringEvent{}
	if err := json.Unmarshal([]byte(eventBytes), &event); err != nil {
		sp.Logger.Error("Error unmarshalling event from heimdall", "error", err)
		return err
	}

	slashInfoHash := hmTypes.ZeroHeimdallHash
	proposerAddr := hmTypes.ZeroHeimdallAddress
	for _, attr := range event.Attributes {
		if attr.Key == slashingTypes.AttributeKeyProposer {
			proposerAddr = hmTypes.HexToHeimdallAddress(attr.Value)
		}
		if attr.Key == slashingTypes.AttributeKeySlashInfoHash {
			slashInfoHash = hmTypes.HexToHeimdallHash(attr.Value)
		}
	}

	sp.Logger.Info("processing tick confirmation event", "eventtype", event.Type, "slashInfoHash", slashInfoHash, "proposer", proposerAddr)
	// TODO - slashing...who should submit tick to rootchain??
	isCurrentProposer, err := util.IsCurrentProposer(sp.cliCtx)
	if err != nil {
		sp.Logger.Error("Error checking isCurrentProposer in CheckpointConfirmation handler", "error", err)
		return err
	}

	// TODO - replace below nonce variable with actual slash tx nonce
	shouldSend, err := sp.shouldSendTickToRootchain(uint64(txHeight))
	if err != nil {
		return err
	}

	// Fetch Tick val slashing info
	tickSlashInfoList, err := sp.fetchTickSlashInfoList()
	if err != nil {
		sp.Logger.Error("Error fetching tick slash info list", "error", err)
		return err
	}

	// Validate tickSlashInfoList
	isValidSlashInfo, err := sp.validateTickSlashInfo(tickSlashInfoList, slashInfoHash)
	if err != nil {
		sp.Logger.Error("Error validating tick slash info list", "error", err)
		return err
	}

	if shouldSend && isValidSlashInfo && isCurrentProposer {
		txHash, err := hex.DecodeString(txHash)
		if err != nil {
			sp.Logger.Error("Error decoding txHash while sending Tick to rootchain", "txHash", txHash, "error", err)
			return err
		}
		if err := sp.createAndSendTickToRootchain(txHeight, txHash); err != nil {
			sp.Logger.Error("Error sending tick to rootchain", "error", err)
			return err
		}
	} else {
		sp.Logger.Info("I am not the current proposer or tick already sent or invalid tick data... Ignoring", "eventType", event.Type)
		return nil
	}
	return nil
}

/*
sendTickAckToHeimdall - sends tick ack msg to heimdall
*/
func (sp *SlashingProcessor) sendTickAckToHeimdall(eventName string, logBytes string) error {
	var log = types.Log{}
	if err := json.Unmarshal([]byte(logBytes), &log); err != nil {
		sp.Logger.Error("Error while unmarshalling event from rootchain", "error", err)
		return err
	}

	event := new(stakinginfo.StakinginfoSlashed)
	if err := helper.UnpackLog(sp.stakingInfoAbi, event, eventName, &log); err != nil {
		sp.Logger.Error("Error while parsing event", "name", eventName, "error", err)
	} else {
		sp.Logger.Info(
			"✅ Received task to send tick-ack to heimdall",
			"event", eventName,
			"totalSlashedAmount", event.Amount,
			"txHash", hmTypes.BytesToHeimdallHash(log.TxHash.Bytes()),
			"logIndex", uint64(log.Index),
		)

		// TODO - check if this ack is already processed on heimdall or not.
		// TODO - check if i am the proposer of this ack or not.

		// create msg checkpoint ack message
		msg := slashingTypes.NewMsgtickAck(helper.GetFromAddress(sp.cliCtx), hmTypes.BytesToHeimdallHash(log.TxHash.Bytes()), uint64(log.Index))

		// return broadcast to heimdall
		if err := sp.txBroadcaster.BroadcastToHeimdall(msg); err != nil {
			sp.Logger.Error("Error while broadcasting tick-ack to heimdall", "error", err)
			return err
		}
	}
	return nil
}

// shouldSendTickToRootchain - verifies if this tick is already submitted to rootchain
func (sp *SlashingProcessor) shouldSendTickToRootchain(tickNonce uint64) (shouldSend bool, err error) {
	/*
		1. Fetch latest tick nonce processed on rootchain.
		2.

	*/

	return
}

// createAndSendTickToRootchain prepares the data required for rootchain tick submission
// and sends a transaction to rootchain
func (sp *SlashingProcessor) createAndSendTickToRootchain(height int64, txHash []byte) error {

	return nil
}

// fetchLatestSlashInfoHash - fetches latest slashInfoHash
func (sp *SlashingProcessor) fetchLatestSlashInfoHash() (slashInfoHash hmTypes.HeimdallHash, err error) {
	sp.Logger.Info("Sending Rest call to Get Latest SlashInfoHash")
	response, err := helper.FetchFromAPI(sp.cliCtx, helper.GetHeimdallServerEndpoint(util.LatestSlashInfoHashURL))
	if err != nil {
		sp.Logger.Error("Error Fetching slashInfoHash from HeimdallServer ", "error", err)
		return slashInfoHash, err
	}
	sp.Logger.Info("Latest SlashInfoHash fetched")
	if err := json.Unmarshal(response.Result, &slashInfoHash); err != nil {
		sp.Logger.Error("Error unmarshalling latest slashinfo hash received from Heimdall Server", "error", err)
		return slashInfoHash, err
	}
	return slashInfoHash, nil
}

// fetchTickSlashInfoList - fetches tick slash Info list
func (sp *SlashingProcessor) fetchTickSlashInfoList() (slashInfoList []*hmTypes.ValidatorSlashingInfo, err error) {
	sp.Logger.Info("Sending Rest call to Get Tick SlashInfo list")
	response, err := helper.FetchFromAPI(sp.cliCtx, helper.GetHeimdallServerEndpoint(util.TickSlashInfoListURL))
	if err != nil {
		sp.Logger.Error("Error Fetching Tick slashInfoList from HeimdallServer ", "error", err)
		return slashInfoList, err
	}
	sp.Logger.Info("Tick SlashInfo List fetched")
	if err := json.Unmarshal(response.Result, &slashInfoList); err != nil {
		sp.Logger.Error("Error unmarshalling tick slashinfo list received from Heimdall Server", "error", err)
		return slashInfoList, err
	}
	return slashInfoList, nil
}

func (sp *SlashingProcessor) validateTickSlashInfo(slashInfoList []*hmTypes.ValidatorSlashingInfo, slashInfoHash hmTypes.HeimdallHash) (isValid bool, err error) {
	tickSlashInfoHash, err := slashingTypes.GenerateInfoHash(slashInfoList)
	if err != nil {
		sp.Logger.Error("Error generating tick slashinfo hash", "error", err)
		return
	}
	// compare tickSlashInfoHash with slashInfoHash
	if bytes.Compare(tickSlashInfoHash, slashInfoHash.Bytes()) == 0 {

		return true, nil
	} else {
		sp.Logger.Info("SlashingInfoHash mismatch", "tickSlashInfoHash", tickSlashInfoHash, "slashInfoHash", slashInfoHash)
	}

	return
}