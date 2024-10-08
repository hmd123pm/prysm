package electra

import (
	"context"

	"github.com/pkg/errors"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/blocks"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/signing"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/state"
	"github.com/prysmaticlabs/prysm/v5/config/params"
	"github.com/prysmaticlabs/prysm/v5/consensus-types/primitives"
	"github.com/prysmaticlabs/prysm/v5/contracts/deposit"
	"github.com/prysmaticlabs/prysm/v5/encoding/bytesutil"
	"github.com/prysmaticlabs/prysm/v5/monitoring/tracing/trace"
	enginev1 "github.com/prysmaticlabs/prysm/v5/proto/engine/v1"
	eth "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	ethpb "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/v5/runtime/version"
	"github.com/prysmaticlabs/prysm/v5/time/slots"
	log "github.com/sirupsen/logrus"
)

// ProcessDeposits is one of the operations performed on each processed
// beacon block to verify queued validators from the Ethereum 1.0 Deposit Contract
// into the beacon chain.
//
// Spec pseudocode definition:
//
//	For each deposit in block.body.deposits:
//	  process_deposit(state, deposit)
func ProcessDeposits(
	ctx context.Context,
	beaconState state.BeaconState,
	deposits []*ethpb.Deposit,
) (state.BeaconState, error) {
	ctx, span := trace.StartSpan(ctx, "electra.ProcessDeposits")
	defer span.End()
	// Attempt to verify all deposit signatures at once, if this fails then fall back to processing
	// individual deposits with signature verification enabled.
	batchVerified, err := blocks.BatchVerifyDepositsSignatures(ctx, deposits)
	if err != nil {
		return nil, errors.Wrap(err, "could not verify deposit signatures in batch")
	}

	for _, d := range deposits {
		if d == nil || d.Data == nil {
			return nil, errors.New("got a nil deposit in block")
		}
		beaconState, err = ProcessDeposit(beaconState, d, batchVerified)
		if err != nil {
			return nil, errors.Wrapf(err, "could not process deposit from %#x", bytesutil.Trunc(d.Data.PublicKey))
		}
	}
	return beaconState, nil
}

// ProcessDeposit takes in a deposit object and inserts it
// into the registry as a new validator or balance change.
// Returns the resulting state, a boolean to indicate whether or not the deposit
// resulted in a new validator entry into the beacon state, and any error.
//
// Spec pseudocode definition:
// def process_deposit(state: BeaconState, deposit: Deposit) -> None:
//
//	# Verify the Merkle branch
//	assert is_valid_merkle_branch(
//		leaf=hash_tree_root(deposit.data),
//		branch=deposit.proof,
//		depth=DEPOSIT_CONTRACT_TREE_DEPTH + 1,  # Add 1 for the List length mix-in
//		index=state.eth1_deposit_index,
//		root=state.eth1_data.deposit_root,
//	)
//
//	 # Deposits must be processed in order
//	 state.eth1_deposit_index += 1
//
//	 apply_deposit(
//	  state=state,
//	  pubkey=deposit.data.pubkey,
//	  withdrawal_credentials=deposit.data.withdrawal_credentials,
//	  amount=deposit.data.amount,
//	  signature=deposit.data.signature,
//	 )
func ProcessDeposit(beaconState state.BeaconState, deposit *ethpb.Deposit, verified bool) (state.BeaconState, error) {
	if err := blocks.VerifyDeposit(beaconState, deposit); err != nil {
		if deposit == nil || deposit.Data == nil {
			return nil, err
		}
		return nil, errors.Wrapf(err, "could not verify deposit from %#x", bytesutil.Trunc(deposit.Data.PublicKey))
	}
	if err := beaconState.SetEth1DepositIndex(beaconState.Eth1DepositIndex() + 1); err != nil {
		return nil, err
	}
	return ApplyDeposit(beaconState, deposit.Data, verified)
}

// ApplyDeposit
// def apply_deposit(state: BeaconState,
//
//	              pubkey: BLSPubkey,
//	              withdrawal_credentials: Bytes32,
//	              amount: uint64,
//	              signature: BLSSignature) -> None:
//	validator_pubkeys = [v.pubkey for v in state.validators]
//	if pubkey not in validator_pubkeys:
//	    # Verify the deposit signature (proof of possession) which is not checked by the deposit contract
//	    if is_valid_deposit_signature(pubkey, withdrawal_credentials, amount, signature):
//	        add_validator_to_registry(state, pubkey, withdrawal_credentials, Gwei(0))  # [Modified in Electra:EIP7251]
//	        # [New in Electra:EIP7251]
//	        state.pending_deposits.append(PendingDeposit(
//	            pubkey=pubkey,
//	            withdrawal_credentials=withdrawal_credentials,
//	            amount=amount,
//	            signature=signature,
//	            slot=GENESIS_SLOT,
//	        ))
//	else:
//	    # Increase balance by deposit amount
//	    # [Modified in Electra:EIP7251]
//	    state.pending_deposits.append(PendingDeposit(
//	        pubkey=pubkey,
//	        withdrawal_credentials=withdrawal_credentials,
//	        amount=amount,
//	        signature=signature,
//	        slot=GENESIS_SLOT
//	    ))
func ApplyDeposit(beaconState state.BeaconState, data *ethpb.Deposit_Data, verified bool) (state.BeaconState, error) {
	pubKey := data.PublicKey
	amount := data.Amount
	withdrawalCredentials := data.WithdrawalCredentials
	signature := data.Signature
	_, ok := beaconState.ValidatorIndexByPubkey(bytesutil.ToBytes48(pubKey))
	if !ok {
		if !verified {
			valid, err := IsValidDepositSignature(data)
			if err != nil {
				return nil, errors.Wrap(err, "could not verify deposit signature")
			}
			if !valid {
				return beaconState, nil
			}
		}

		if err := AddValidatorToRegistry(beaconState, pubKey, withdrawalCredentials, 0); err != nil { // # [Modified in Electra:EIP7251]
			return nil, errors.Wrap(err, "could not add validator to registry")
		}
	}
	// no validation on top-ups (phase0 feature). no validation before state change
	if err := beaconState.AppendPendingDeposit(&ethpb.PendingDeposit{
		PublicKey:             pubKey,
		WithdrawalCredentials: withdrawalCredentials,
		Amount:                amount,
		Signature:             signature,
		Slot:                  params.BeaconConfig().GenesisSlot,
	}); err != nil {
		return nil, err
	}
	return beaconState, nil
}

// IsValidDepositSignature returns whether deposit_data is valid
// def is_valid_deposit_signature(pubkey: BLSPubkey, withdrawal_credentials: Bytes32, amount: uint64, signature: BLSSignature) -> bool:
//
//	deposit_message = DepositMessage( pubkey=pubkey, withdrawal_credentials=withdrawal_credentials, amount=amount, )
//	domain = compute_domain(DOMAIN_DEPOSIT)  # Fork-agnostic domain since deposits are valid across forks
//	signing_root = compute_signing_root(deposit_message, domain)
//	return bls.Verify(pubkey, signing_root, signature)
func IsValidDepositSignature(data *ethpb.Deposit_Data) (bool, error) {
	domain, err := signing.ComputeDomain(params.BeaconConfig().DomainDeposit, nil, nil)
	if err != nil {
		return false, err
	}
	if err := verifyDepositDataSigningRoot(data, domain); err != nil {
		// Ignore this error as in the spec pseudo code.
		log.WithError(err).Debug("Skipping deposit: could not verify deposit data signature")
		return false, nil
	}
	return true, nil
}

func verifyDepositDataSigningRoot(obj *ethpb.Deposit_Data, domain []byte) error {
	return deposit.VerifyDepositSignature(obj, domain)
}

// ProcessPendingDeposits implements the spec definition below. This method mutates the state.
// Iterating over `pending_deposits` queue this function runs the following checks before applying pending deposit:
// 1. All Eth1 bridge deposits are processed before the first deposit request gets processed.
// 2. Deposit position in the queue is finalized.
// 3. Deposit does not exceed the `MAX_PENDING_DEPOSITS_PER_EPOCH` limit.
// 4. Deposit does not exceed the activation churn limit.
//
// Spec definition:
//
//		def process_pending_deposits(state: BeaconState) -> None:
//	   available_for_processing = state.deposit_balance_to_consume + get_activation_exit_churn_limit(state)
//	   processed_amount = 0
//	   next_deposit_index = 0
//	   deposits_to_postpone = []
//	   is_churn_limit_reached = False
//	   finalized_slot = compute_start_slot_at_epoch(state.finalized_checkpoint.epoch)
//
//	   for deposit in state.pending_deposits:
//	       # Do not process deposit requests if Eth1 bridge deposits are not yet applied.
//	       if (
//	           # Is deposit request
//	           deposit.slot > GENESIS_SLOT and
//	           # There are pending Eth1 bridge deposits
//	           state.eth1_deposit_index < state.deposit_requests_start_index
//	       ):
//	           break
//
//	       # Check if deposit has been finalized, otherwise, stop processing.
//	       if deposit.slot > finalized_slot:
//	           break
//
//	       # Check if number of processed deposits has not reached the limit, otherwise, stop processing.
//	       if next_deposit_index >= MAX_PENDING_DEPOSITS_PER_EPOCH:
//	           break
//
//	       # Read validator state
//	       is_validator_exited = False
//	       is_validator_withdrawn = False
//	       validator_pubkeys = [v.pubkey for v in state.validators]
//	       if deposit.pubkey in validator_pubkeys:
//	           validator = state.validators[ValidatorIndex(validator_pubkeys.index(deposit.pubkey))]
//	           is_validator_exited = validator.exit_epoch < FAR_FUTURE_EPOCH
//	           is_validator_withdrawn = validator.withdrawable_epoch < get_current_epoch(state)
//
//	       if is_validator_withdrawn:
//	           # Deposited balance will never become active. Increase balance but do not consume churn
//	           apply_pending_deposit(state, deposit)
//	       elif is_validator_exited:
//	           # Validator is exiting, postpone the deposit until after withdrawable epoch
//	           deposits_to_postpone.append(deposit)
//	       else:
//	           # Check if deposit fits in the churn, otherwise, do no more deposit processing in this epoch.
//	           is_churn_limit_reached = processed_amount + deposit.amount > available_for_processing
//	           if is_churn_limit_reached:
//	               break
//
//	           # Consume churn and apply deposit.
//	           processed_amount += deposit.amount
//	           apply_pending_deposit(state, deposit)
//
//	       # Regardless of how the deposit was handled, we move on in the queue.
//	       next_deposit_index += 1
//
//	   state.pending_deposits = state.pending_deposits[next_deposit_index:] + deposits_to_postpone
//
//	   # Accumulate churn only if the churn limit has been hit.
//	   if is_churn_limit_reached:
//	       state.deposit_balance_to_consume = available_for_processing - processed_amount
//	   else:
//	       state.deposit_balance_to_consume = Gwei(0)
func ProcessPendingDeposits(ctx context.Context, st state.BeaconState, activeBalance primitives.Gwei) error {
	_, span := trace.StartSpan(ctx, "electra.ProcessPendingDeposits")
	defer span.End()

	if st == nil || st.IsNil() {
		return errors.New("nil state")
	}

	depBalToConsume, err := st.DepositBalanceToConsume()
	if err != nil {
		return err
	}
	availableForProcessing := depBalToConsume + helpers.ActivationExitChurnLimit(activeBalance)
	processedAmount := uint64(0)
	nextDepositIndex := uint64(0)
	var depositsToPostpone []*eth.PendingDeposit

	deposits, err := st.PendingDeposits()
	if err != nil {
		return err
	}
	isChurnLimitReached := false
	finalizedSlot, err := slots.EpochStart(st.FinalizedCheckpoint().Epoch)
	if err != nil {
		return errors.Wrap(err, "could not get finalized slot")
	}
	// constants
	ffe := params.BeaconConfig().FarFutureEpoch
	curEpoch := slots.ToEpoch(st.Slot())

	// Slice to collect deposits needing signature verification
	var depositsToVerify []*ethpb.PendingDeposit
	for _, pendingDeposit := range deposits {
		startIndex, err := st.DepositRequestsStartIndex()
		if err != nil {
			return errors.Wrap(err, "could not get starting pendingDeposit index")
		}

		// Do not process pendingDeposit requests if Eth1 bridge deposits are not yet applied.
		if pendingDeposit.Slot > params.BeaconConfig().GenesisSlot && st.Eth1DepositIndex() < startIndex {
			break
		}

		// Check if pendingDeposit has been finalized, otherwise, stop processing.
		if pendingDeposit.Slot > finalizedSlot {
			break
		}

		// Check if number of processed deposits has not reached the limit, otherwise, stop processing.
		if nextDepositIndex >= params.BeaconConfig().MaxPendingDepositsPerEpoch {
			break
		}

		var isValidatorExited bool
		var isValidatorWithdrawn bool
		index, found := st.ValidatorIndexByPubkey(bytesutil.ToBytes48(pendingDeposit.PublicKey))
		if found {
			val, err := st.ValidatorAtIndexReadOnly(index)
			if err != nil {
				return errors.Wrap(err, "could not get validator")
			}
			isValidatorExited = val.ExitEpoch() < ffe
			isValidatorWithdrawn = val.WithdrawableEpoch() < curEpoch
		}

		if isValidatorWithdrawn {
			// Deposited balance will never become active. Increase balance but do not consume churn
			if err := ApplyPendingDeposit(ctx, st, pendingDeposit); err != nil {
				return errors.Wrap(err, "could not apply pending deposit")
			}
		} else if isValidatorExited {
			// Validator is exiting, postpone the pendingDeposit until after withdrawable epoch
			depositsToPostpone = append(depositsToPostpone, pendingDeposit)
		} else {
			// Check if pendingDeposit fits in the churn, otherwise, do no more pendingDeposit processing in this epoch.
			isChurnLimitReached = primitives.Gwei(processedAmount+pendingDeposit.Amount) > availableForProcessing
			if isChurnLimitReached { // Is churn limit reached?
				break
			}
			// Consume churn and apply pendingDeposit.
			processedAmount += pendingDeposit.Amount
			if found {
				if err := helpers.IncreaseBalance(st, index, pendingDeposit.Amount); err != nil {
					return errors.Wrap(err, "could not increase balance")
				}
			} else {
				// Collect deposit for batch signature verification
				depositsToVerify = append(depositsToVerify, pendingDeposit)
			}
		}

		// Regardless of how the pendingDeposit was handled, we move on in the queue.
		nextDepositIndex++
	}

	// Perform batch signature verification on deposits on unfound validators
	if err = batchProcessPendingDeposts(ctx, st, depositsToPostpone); err != nil {
		return errors.Wrap(err, "could not process pending deposits with new public keys")
	}

	// Combined operation:
	// - state.pending_deposits = state.pending_deposits[next_deposit_index:]
	// - state.pending_deposits += deposits_to_postpone
	// However, the number of remaining deposits must be maintained to properly update the pendingDeposit
	// balance to consume.
	deposits = append(deposits[nextDepositIndex:], depositsToPostpone...)
	if err := st.SetPendingDeposits(deposits); err != nil {
		return errors.Wrap(err, "could not set pending deposits")
	}
	// Accumulate churn only if the churn limit has been hit.
	if isChurnLimitReached {
		return st.SetDepositBalanceToConsume(availableForProcessing - primitives.Gwei(processedAmount))
	}
	return st.SetDepositBalanceToConsume(0)
}

func batchProcessPendingDeposts(ctx context.Context, st state.BeaconState, depositsToVerify []*ethpb.PendingDeposit) error {
	if len(depositsToVerify) > 0 {
		verified, err := blocks.BatchVerifyPendingDepositsSignatures(ctx, depositsToVerify)
		if err != nil {
			return errors.Wrap(err, "could not batch verify deposit signatures")
		}
		for _, dep := range depositsToVerify {
			if !verified {
				verified, err = blocks.IsValidDepositSignature(&ethpb.Deposit_Data{
					PublicKey:             bytesutil.SafeCopyBytes(dep.PublicKey),
					WithdrawalCredentials: bytesutil.SafeCopyBytes(dep.WithdrawalCredentials),
					Amount:                dep.Amount,
					Signature:             bytesutil.SafeCopyBytes(dep.Signature),
				})
				if err != nil {
					return errors.Wrap(err, "could not verify deposit signature")
				}
			}
			if verified {
				if err := AddValidatorToRegistry(st, dep.PublicKey, dep.WithdrawalCredentials, dep.Amount); err != nil {
					return errors.Wrap(err, "could not add validator to registry")
				}
			}
		}
	}
	return nil
}

// ApplyPendingDeposit implements the spec definition below.
//
// Spec Definition:
//
// def apply_pending_deposit(state: BeaconState, deposit: PendingDeposit) -> None:
//
//	"""
//	Applies ``deposit`` to the ``state``.
//	"""
//	validator_pubkeys = [v.pubkey for v in state.validators]
//	if deposit.pubkey not in validator_pubkeys:
//	    # Verify the deposit signature (proof of possession) which is not checked by the deposit contract
//	    if is_valid_deposit_signature(
//	        deposit.pubkey,
//	        deposit.withdrawal_credentials,
//	        deposit.amount,
//	        deposit.signature
//	    ):
//	        add_validator_to_registry(state, deposit.pubkey, deposit.withdrawal_credentials, deposit.amount)
//	else:
//	    validator_index = ValidatorIndex(validator_pubkeys.index(deposit.pubkey))
//	    # Increase balance
//	    increase_balance(state, validator_index, deposit.amount)
func ApplyPendingDeposit(ctx context.Context, st state.BeaconState, deposit *ethpb.PendingDeposit) error {
	_, span := trace.StartSpan(ctx, "electra.ApplyPendingDeposit")
	defer span.End()
	index, ok := st.ValidatorIndexByPubkey(bytesutil.ToBytes48(deposit.PublicKey))
	if !ok {
		verified, err := blocks.IsValidDepositSignature(&ethpb.Deposit_Data{
			PublicKey:             bytesutil.SafeCopyBytes(deposit.PublicKey),
			WithdrawalCredentials: bytesutil.SafeCopyBytes(deposit.WithdrawalCredentials),
			Amount:                deposit.Amount,
			Signature:             bytesutil.SafeCopyBytes(deposit.Signature),
		})
		if err != nil {
			return errors.Wrap(err, "could not verify deposit signature")
		}

		if verified {
			if err := AddValidatorToRegistry(st, deposit.PublicKey, deposit.WithdrawalCredentials, deposit.Amount); err != nil {
				return errors.Wrap(err, "could not add validator to registry")
			}
		}
		return nil
	}
	return helpers.IncreaseBalance(st, index, deposit.Amount)
}

// AddValidatorToRegistry updates the beacon state with validator information
// def add_validator_to_registry(state: BeaconState, pubkey: BLSPubkey, withdrawal_credentials: Bytes32, amount: uint64) -> None:
//
//	index = get_index_for_new_validator(state)
//	validator = get_validator_from_deposit(pubkey, withdrawal_credentials, amount)  # [Modified in Electra:EIP7251]
//	set_or_append_list(state.validators, index, validator)
//	set_or_append_list(state.balances, index, amount)
//	set_or_append_list(state.previous_epoch_participation, index, ParticipationFlags(0b0000_0000))
//	set_or_append_list(state.current_epoch_participation, index, ParticipationFlags(0b0000_0000))
//	set_or_append_list(state.inactivity_scores, index, uint64(0))
func AddValidatorToRegistry(beaconState state.BeaconState, pubKey []byte, withdrawalCredentials []byte, amount uint64) error {
	val := GetValidatorFromDeposit(pubKey, withdrawalCredentials, amount)
	if err := beaconState.AppendValidator(val); err != nil {
		return err
	}
	if err := beaconState.AppendBalance(amount); err != nil {
		return err
	}

	// only active in altair and only when it's a new validator (after append balance)
	if beaconState.Version() >= version.Altair {
		if err := beaconState.AppendInactivityScore(0); err != nil {
			return err
		}
		if err := beaconState.AppendPreviousParticipationBits(0); err != nil {
			return err
		}
		if err := beaconState.AppendCurrentParticipationBits(0); err != nil {
			return err
		}
	}
	return nil
}

// GetValidatorFromDeposit gets a new validator object with provided parameters
//
// def get_validator_from_deposit(pubkey: BLSPubkey, withdrawal_credentials: Bytes32, amount: uint64) -> Validator:
//
//	validator = Validator(
//	    pubkey=pubkey,
//	    withdrawal_credentials=withdrawal_credentials,
//	    activation_eligibility_epoch=FAR_FUTURE_EPOCH,
//	    activation_epoch=FAR_FUTURE_EPOCH,
//	    exit_epoch=FAR_FUTURE_EPOCH,
//	    withdrawable_epoch=FAR_FUTURE_EPOCH,
//	    effective_balance=Gwei(0),
//	)
//
//	# [Modified in Electra:EIP7251]
//	max_effective_balance = get_max_effective_balance(validator)
//	validator.effective_balance = min(amount - amount % EFFECTIVE_BALANCE_INCREMENT, max_effective_balance)
//
//	return validator
func GetValidatorFromDeposit(pubKey []byte, withdrawalCredentials []byte, amount uint64) *ethpb.Validator {
	validator := &ethpb.Validator{
		PublicKey:                  pubKey,
		WithdrawalCredentials:      withdrawalCredentials,
		ActivationEligibilityEpoch: params.BeaconConfig().FarFutureEpoch,
		ActivationEpoch:            params.BeaconConfig().FarFutureEpoch,
		ExitEpoch:                  params.BeaconConfig().FarFutureEpoch,
		WithdrawableEpoch:          params.BeaconConfig().FarFutureEpoch,
		EffectiveBalance:           0,
	}
	maxEffectiveBalance := helpers.ValidatorMaxEffectiveBalance(validator)
	validator.EffectiveBalance = min(amount-(amount%params.BeaconConfig().EffectiveBalanceIncrement), maxEffectiveBalance)
	return validator
}

// ProcessDepositRequests is a function as part of electra to process execution layer deposits
func ProcessDepositRequests(ctx context.Context, beaconState state.BeaconState, requests []*enginev1.DepositRequest) (state.BeaconState, error) {
	_, span := trace.StartSpan(ctx, "electra.ProcessDepositRequests")
	defer span.End()

	if len(requests) == 0 {
		return beaconState, nil
	}

	var err error
	for _, receipt := range requests {
		beaconState, err = processDepositRequest(beaconState, receipt)
		if err != nil {
			return nil, errors.Wrap(err, "could not apply deposit request")
		}
	}
	return beaconState, nil
}

// processDepositRequest processes the specific deposit receipt
// def process_deposit_request(state: BeaconState, deposit_request: DepositRequest) -> None:
//
//		# Set deposit request start index
//		if state.deposit_requests_start_index == UNSET_DEPOSIT_REQUEST_START_INDEX:
//		    state.deposit_requests_start_index = deposit_request.index
//
//		state.pending_deposits.append(PendingDeposit(
//	       pubkey=deposit_request.pubkey,
//	       withdrawal_credentials=deposit_request.withdrawal_credentials,
//	       amount=deposit_request.amount,
//	       signature=deposit_request.signature,
//	       slot=state.slot,
//	   ))
func processDepositRequest(beaconState state.BeaconState, request *enginev1.DepositRequest) (state.BeaconState, error) {
	requestsStartIndex, err := beaconState.DepositRequestsStartIndex()
	if err != nil {
		return nil, errors.Wrap(err, "could not get deposit requests start index")
	}
	if requestsStartIndex == params.BeaconConfig().UnsetDepositRequestsStartIndex {
		if request == nil {
			return nil, errors.New("nil deposit request")
		}
		if err := beaconState.SetDepositRequestsStartIndex(request.Index); err != nil {
			return nil, errors.Wrap(err, "could not set deposit requests start index")
		}
	}
	if err := beaconState.AppendPendingDeposit(&ethpb.PendingDeposit{
		PublicKey:             bytesutil.SafeCopyBytes(request.Pubkey),
		Amount:                request.Amount,
		WithdrawalCredentials: bytesutil.SafeCopyBytes(request.WithdrawalCredentials),
		Signature:             bytesutil.SafeCopyBytes(request.Signature),
		Slot:                  beaconState.Slot(),
	}); err != nil {
		return nil, errors.Wrap(err, "could not append deposit request")
	}
	return beaconState, nil
}
