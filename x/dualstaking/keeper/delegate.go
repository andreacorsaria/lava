package keeper

// Delegation allows securing funds for a specific provider to effectively increase
// its stake so it will be paired with consumers more often. The delegators do not
// transfer the funds to the provider but only bestow the funds with it. In return
// to locking the funds there, delegators get some of the provider’s profit (after
// commission deduction).
//
// The delegated funds are stored in the module's BondedPoolName account. On request
// to terminate the delegation, they are then moved to the modules NotBondedPoolName
// account, and remain locked there for staking.UnbondingTime() witholding period
// before finally released back to the delegator. The timers for bonded funds are
// tracked are indexed by the delegator, provider, and chainID.
//
// The delegation state is stores with fixation using two maps: one for delegations
// indexed by the combination <provider,chainD,delegator>, used to track delegations
// and find/access delegations by provider (and chainID); and another for delegators
// tracking the list of providers for a delegator, indexed by the delegator.

import (
	"fmt"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/lavanet/lava/utils"
	lavaslices "github.com/lavanet/lava/utils/slices"
	"github.com/lavanet/lava/x/dualstaking/types"
	epochstoragetypes "github.com/lavanet/lava/x/epochstorage/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
	"golang.org/x/exp/slices"
)

const (
	EMPTY_PROVIDER         = "empty_provider"
	EMPTY_PROVIDER_CHAINID = ""
)

// validateCoins validates that the input amount is valid and non-negative
func validateCoins(amount sdk.Coin) error {
	if !amount.IsValid() {
		return utils.LavaFormatWarning("invalid coins to delegate",
			sdkerrors.ErrInvalidCoins,
			utils.Attribute{Key: "amount", Value: amount},
		)
	}
	return nil
}

// increaseDelegation increases the delegation of a delegator to a provider for a
// given chain. It updates the fixation stores for both delegations and delegators,
// and updates the (epochstorage) stake-entry.
func (k Keeper) increaseDelegation(ctx sdk.Context, delegator, provider, chainID string, amount sdk.Coin, nextEpoch uint64) error {
	// get, update and append the delegation entry
	var delegationEntry types.Delegation
	index := types.DelegationKey(provider, delegator, chainID)
	found := k.delegationFS.FindEntry(ctx, index, nextEpoch, &delegationEntry)
	if !found {
		// new delegation (i.e. not increase of existing one)
		delegationEntry = types.NewDelegation(delegator, provider, chainID)
	}

	delegationEntry.AddAmount(amount)

	err := k.delegationFS.AppendEntry(ctx, index, nextEpoch, &delegationEntry)
	if err != nil {
		// append should never fail here
		return utils.LavaFormatError("critical: append delegation entry", err,
			utils.Attribute{Key: "delegator", Value: delegationEntry.Delegator},
			utils.Attribute{Key: "provider", Value: delegationEntry.Provider},
			utils.Attribute{Key: "chainID", Value: delegationEntry.ChainID},
		)
	}

	// get, update and append the delegator entry
	var delegatorEntry types.Delegator
	index = types.DelegatorKey(delegator)
	_ = k.delegatorFS.FindEntry(ctx, index, nextEpoch, &delegatorEntry)

	delegatorEntry.AddProvider(provider)

	err = k.delegatorFS.AppendEntry(ctx, index, nextEpoch, &delegatorEntry)
	if err != nil {
		// append should never fail here
		return utils.LavaFormatError("critical: append delegator entry", err,
			utils.Attribute{Key: "delegator", Value: delegator},
			utils.Attribute{Key: "provider", Value: provider},
			utils.Attribute{Key: "chainID", Value: chainID},
		)
	}

	if provider != EMPTY_PROVIDER {
		// update the stake entry
		err = k.increaseStakeEntryDelegation(ctx, delegator, provider, chainID, amount)
		if err != nil {
			return err
		}
	}

	return nil
}

// decreaseDelegation decreases the delegation of a delegator to a provider for a
// given chain. It updates the fixation stores for both delegations and delegators,
// and updates the (epochstorage) stake-entry.
func (k Keeper) decreaseDelegation(ctx sdk.Context, delegator, provider, chainID string, amount sdk.Coin, nextEpoch uint64, unstake bool) error {
	// get, update and append the delegation entry
	var delegationEntry types.Delegation
	index := types.DelegationKey(provider, delegator, chainID)
	found := k.delegationFS.FindEntry(ctx, index, nextEpoch, &delegationEntry)
	if !found {
		return types.ErrDelegationNotFound
	}

	if delegationEntry.Amount.IsLT(amount) {
		return types.ErrInsufficientDelegation
	}

	delegationEntry.SubAmount(amount)

	// if delegation now becomes zero, then remove this entry altogether;
	// otherwise just append the new version (for next epoch).
	if delegationEntry.Amount.IsZero() {
		err := k.delegationFS.DelEntry(ctx, index, nextEpoch)
		if err != nil {
			// delete should never fail here
			return utils.LavaFormatError("critical: delete delegation entry", err,
				utils.Attribute{Key: "delegator", Value: delegator},
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chainID", Value: chainID},
			)
		}
	} else {
		err := k.delegationFS.AppendEntry(ctx, index, nextEpoch, &delegationEntry)
		if err != nil {
			// append should never fail here
			return utils.LavaFormatError("failed to update delegation entry", err,
				utils.Attribute{Key: "delegator", Value: delegator},
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chainID", Value: chainID},
			)
		}
	}

	// get, update and append the delegator entry
	var delegatorEntry types.Delegator
	index = types.DelegatorKey(delegator)
	found = k.delegatorFS.FindEntry(ctx, index, nextEpoch, &delegatorEntry)
	if !found {
		// we found the delegation above, so the delegator must exist as well
		return utils.LavaFormatError("critical: delegator entry for delegation not found",
			types.ErrDelegationNotFound,
			utils.Attribute{Key: "delegator", Value: delegator},
			utils.Attribute{Key: "provider", Value: provider},
			utils.Attribute{Key: "chainID", Value: chainID},
		)
	}

	// if delegation now becomes zero, then remove this provider from the delegator
	// entry; and if the delegator entry becomes entry then remove it altogether.
	// otherwise just append the new version (for next epoch).
	if delegationEntry.Amount.IsZero() {
		delegatorEntry.DelProvider(provider)
		if delegatorEntry.IsEmpty() {
			err := k.delegatorFS.DelEntry(ctx, index, nextEpoch)
			if err != nil {
				// delete should never fail here
				return utils.LavaFormatError("critical: delete delegator entry", err,
					utils.Attribute{Key: "delegator", Value: delegator},
					utils.Attribute{Key: "provider", Value: provider},
					utils.Attribute{Key: "chainID", Value: chainID},
				)
			}
		}
	} else {
		delegatorEntry.AddProvider(provider)
		err := k.delegatorFS.AppendEntry(ctx, index, nextEpoch, &delegatorEntry)
		if err != nil {
			// append should never fail here
			return utils.LavaFormatError("failed to update delegator entry", err,
				utils.Attribute{Key: "delegator", Value: delegator},
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chainID", Value: chainID},
			)
		}
	}

	if provider != EMPTY_PROVIDER {
		if err := k.decreaseStakeEntryDelegation(ctx, delegator, provider, chainID, amount, unstake); err != nil {
			return err
		}
	}

	return nil
}

// increaseStakeEntryDelegation increases the (epochstorage) stake-entry of the provider for a chain.
func (k Keeper) increaseStakeEntryDelegation(ctx sdk.Context, delegator, provider, chainID string, amount sdk.Coin) error {
	providerAddr, err := sdk.AccAddressFromBech32(provider)
	if err != nil {
		// panic:ok: this call was alreadys successful by the caller
		utils.LavaFormatPanic("increaseStakeEntry: invalid provider address", err,
			utils.Attribute{Key: "provider", Value: provider},
		)
	}

	stakeEntry, exists, index := k.epochstorageKeeper.GetStakeEntryByAddressCurrent(ctx, chainID, providerAddr)
	if !exists {
		return epochstoragetypes.ErrProviderNotStaked
	}

	// sanity check
	if stakeEntry.Address != provider {
		return utils.LavaFormatError("critical: delegate to provider with address mismatch", sdkerrors.ErrInvalidAddress,
			utils.Attribute{Key: "provider", Value: provider},
			utils.Attribute{Key: "address", Value: stakeEntry.Address},
		)
	}

	if delegator == provider {
		stakeEntry.Stake = stakeEntry.Stake.Add(amount)
	} else {
		stakeEntry.DelegateTotal = stakeEntry.DelegateTotal.Add(amount)
	}

	k.epochstorageKeeper.ModifyStakeEntryCurrent(ctx, chainID, stakeEntry, index)

	return nil
}

// decreaseStakeEntryDelegation decreases the (epochstorage) stake-entry of the provider for a chain.
func (k Keeper) decreaseStakeEntryDelegation(ctx sdk.Context, delegator, provider, chainID string, amount sdk.Coin, unstake bool) error {
	providerAddr, err := sdk.AccAddressFromBech32(provider)
	if err != nil {
		// panic:ok: this call was alreadys successful by the caller
		utils.LavaFormatPanic("decreaseStakeEntryDelegation: invalid provider address", err,
			utils.Attribute{Key: "provider", Value: provider},
		)
	}

	stakeEntry, exists, index := k.epochstorageKeeper.GetStakeEntryByAddressCurrent(ctx, chainID, providerAddr)
	if !exists {
		return nil
	}

	// sanity check
	if stakeEntry.Address != provider {
		return utils.LavaFormatError("critical: un-delegate from provider with address mismatch", sdkerrors.ErrInvalidAddress,
			utils.Attribute{Key: "provider", Value: provider},
			utils.Attribute{Key: "address", Value: stakeEntry.Address},
		)
	}

	if delegator == provider {
		stakeEntry.Stake, err = stakeEntry.Stake.SafeSub(amount)
		if err != nil {
			return fmt.Errorf("invalid or insufficient funds: %w", err)
		}
		if !unstake && stakeEntry.Stake.IsLT(k.getMinStake(ctx, chainID)) {
			return fmt.Errorf("provider self unbond to less than min stake")
		}
	} else {
		stakeEntry.DelegateTotal, err = stakeEntry.DelegateTotal.SafeSub(amount)
		if err != nil {
			return fmt.Errorf("invalid or insufficient funds: %w", err)
		}
	}

	k.epochstorageKeeper.ModifyStakeEntryCurrent(ctx, chainID, stakeEntry, index)

	return nil
}

// Delegate lets a delegator delegate an amount of coins to a provider.
// (effective on next epoch)
func (k Keeper) Delegate(ctx sdk.Context, delegator, provider, chainID string, amount sdk.Coin) error {
	nextEpoch := k.epochstorageKeeper.GetCurrentNextEpoch(ctx)

	_, err := sdk.AccAddressFromBech32(delegator)
	if err != nil {
		return utils.LavaFormatWarning("invalid delegator address", err,
			utils.Attribute{Key: "delegator", Value: delegator},
		)
	}

	if provider != EMPTY_PROVIDER {
		if _, err = sdk.AccAddressFromBech32(provider); err != nil {
			return utils.LavaFormatWarning("invalid provider address", err,
				utils.Attribute{Key: "provider", Value: provider},
			)
		}
	}

	if err := validateCoins(amount); err != nil {
		return err
	} else if amount.IsZero() {
		return nil
	}

	err = k.increaseDelegation(ctx, delegator, provider, chainID, amount, nextEpoch)
	if err != nil {
		return utils.LavaFormatWarning("failed to increase delegation", err,
			utils.Attribute{Key: "delegator", Value: delegator},
			utils.Attribute{Key: "provider", Value: provider},
			utils.Attribute{Key: "amount", Value: amount.String()},
			utils.Attribute{Key: "chainID", Value: chainID},
		)
	}

	return nil
}

// Redelegate lets a delegator transfer its delegation between providers, but
// without the funds being subject to unstakeHoldBlocks witholding period.
// (effective on next epoch)
func (k Keeper) Redelegate(ctx sdk.Context, delegator, from, to, fromChainID, toChainID string, amount sdk.Coin) error {
	nextEpoch := k.epochstorageKeeper.GetCurrentNextEpoch(ctx)

	if _, err := sdk.AccAddressFromBech32(delegator); err != nil {
		return utils.LavaFormatWarning("invalid delegator address", err,
			utils.Attribute{Key: "delegator", Value: delegator},
		)
	}

	if from != EMPTY_PROVIDER {
		if _, err := sdk.AccAddressFromBech32(from); err != nil {
			return utils.LavaFormatWarning("invalid from-provider address", err,
				utils.Attribute{Key: "from_provider", Value: from},
			)
		}
	}

	if to != EMPTY_PROVIDER_CHAINID {
		if _, err := sdk.AccAddressFromBech32(to); err != nil {
			return utils.LavaFormatWarning("invalid to-provider address", err,
				utils.Attribute{Key: "to_provider", Value: to},
			)
		}
	}

	if err := validateCoins(amount); err != nil {
		return err
	} else if amount.IsZero() {
		return nil
	}

	err := k.increaseDelegation(ctx, delegator, to, toChainID, amount, nextEpoch)
	if err != nil {
		return utils.LavaFormatWarning("failed to increase delegation", err,
			utils.Attribute{Key: "delegator", Value: delegator},
			utils.Attribute{Key: "provider", Value: to},
			utils.Attribute{Key: "amount", Value: amount.String()},
		)
	}

	err = k.decreaseDelegation(ctx, delegator, from, fromChainID, amount, nextEpoch, false)
	if err != nil {
		return utils.LavaFormatWarning("failed to decrease delegation", err,
			utils.Attribute{Key: "delegator", Value: delegator},
			utils.Attribute{Key: "provider", Value: from},
			utils.Attribute{Key: "amount", Value: amount.String()},
		)
	}

	// no need to transfer funds, because they remain in the dualstaking module
	// (specifically in types.BondedPoolName).

	return nil
}

// Unbond lets a delegator get its delegated coins back from a provider. The
// delegation ends immediately, but coins are held for unstakeHoldBlocks period
// before released and transferred back to the delegator. The rewards from the
// provider will be updated accordingly (or terminate) from the next epoch.
// (effective on next epoch)
func (k Keeper) Unbond(ctx sdk.Context, delegator, provider, chainID string, amount sdk.Coin, unstake bool) error {
	nextEpoch := k.epochstorageKeeper.GetCurrentNextEpoch(ctx)

	if _, err := sdk.AccAddressFromBech32(delegator); err != nil {
		return utils.LavaFormatWarning("invalid delegator address", err,
			utils.Attribute{Key: "delegator", Value: delegator},
		)
	}

	if provider != EMPTY_PROVIDER {
		if _, err := sdk.AccAddressFromBech32(provider); err != nil {
			return utils.LavaFormatWarning("invalid provider address", err,
				utils.Attribute{Key: "provider", Value: provider},
			)
		}
	}

	if err := validateCoins(amount); err != nil {
		return err
	} else if amount.IsZero() {
		return nil
	}

	err := k.decreaseDelegation(ctx, delegator, provider, chainID, amount, nextEpoch, unstake)
	if err != nil {
		return utils.LavaFormatWarning("failed to decrease delegation", err,
			utils.Attribute{Key: "delegator", Value: delegator},
			utils.Attribute{Key: "provider", Value: provider},
			utils.Attribute{Key: "amount", Value: amount.String()},
		)
	}

	return nil
}

func (k Keeper) getUnbondHoldBlocks(ctx sdk.Context, chainID string) uint64 {
	_, found, providerType := k.specKeeper.IsSpecFoundAndActive(ctx, chainID)
	if !found {
		utils.LavaFormatError("critical: failed to get spec for chainID",
			fmt.Errorf("unknown chainID"),
			utils.Attribute{Key: "chainID", Value: chainID},
		)
	}

	// note: if spec was not found, the default choice is Spec_dynamic == 0

	block := uint64(ctx.BlockHeight())
	if providerType == spectypes.Spec_static {
		return k.epochstorageKeeper.UnstakeHoldBlocksStatic(ctx, block)
	} else {
		return k.epochstorageKeeper.UnstakeHoldBlocks(ctx, block)
	}

	// NOT REACHED
}

func (k Keeper) getMinStake(ctx sdk.Context, chainID string) sdk.Coin {
	spec, found := k.specKeeper.GetSpec(ctx, chainID)
	if !found {
		utils.LavaFormatError("critical: failed to get spec for chainID",
			fmt.Errorf("unknown chainID"),
			utils.Attribute{Key: "chainID", Value: chainID},
		)
	}

	return spec.MinStakeProvider
}

// GetDelegatorProviders gets all the providers the delegator is delegated to
func (k Keeper) GetDelegatorProviders(ctx sdk.Context, delegator string, epoch uint64) (providers []string, err error) {
	_, err = sdk.AccAddressFromBech32(delegator)
	if err != nil {
		return nil, utils.LavaFormatWarning("cannot get delegator's providers", err,
			utils.Attribute{Key: "delegator", Value: delegator},
		)
	}

	var delegatorEntry types.Delegator
	prefix := types.DelegatorKey(delegator)
	k.delegatorFS.FindEntry(ctx, prefix, epoch, &delegatorEntry)

	return delegatorEntry.Providers, nil
}

func (k Keeper) GetProviderDelegators(ctx sdk.Context, provider string, epoch uint64) ([]types.Delegation, error) {
	if provider != EMPTY_PROVIDER {
		_, err := sdk.AccAddressFromBech32(provider)
		if err != nil {
			return nil, utils.LavaFormatWarning("cannot get provider's delegators", err,
				utils.Attribute{Key: "provider", Value: provider},
			)
		}
	}

	var delegations []types.Delegation
	indices := k.delegationFS.GetAllEntryIndicesWithPrefix(ctx, provider)
	for _, ind := range indices {
		var delegation types.Delegation
		found := k.delegationFS.FindEntry(ctx, ind, epoch, &delegation)
		if !found {
			provider, delegator, chainID := types.DelegationKeyDecode(ind)
			utils.LavaFormatError("delegationFS entry index has no entry", fmt.Errorf("provider delegation not found"),
				utils.Attribute{Key: "delegator", Value: delegator},
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chainID", Value: chainID},
			)
			continue
		}
		delegations = append(delegations, delegation)
	}

	return delegations, nil
}

func (k Keeper) GetDelegation(ctx sdk.Context, delegator, provider, chainID string, epoch uint64) (types.Delegation, bool) {
	var delegationEntry types.Delegation
	index := types.DelegationKey(provider, delegator, chainID)
	found := k.delegationFS.FindEntry(ctx, index, epoch, &delegationEntry)

	return delegationEntry, found
}

func (k Keeper) GetAllProviderDelegatorDelegations(ctx sdk.Context, delegator, provider string, epoch uint64) []types.Delegation {
	prefix := types.DelegationKey(provider, delegator, "")
	indices := k.delegationFS.GetAllEntryIndicesWithPrefix(ctx, prefix)

	var delegations []types.Delegation
	for _, ind := range indices {
		var delegation types.Delegation
		found := k.delegationFS.FindEntry(ctx, ind, epoch, &delegation)
		if !found {
			provider, delegator, chainID := types.DelegationKeyDecode(ind)
			utils.LavaFormatError("delegationFS entry index has no entry", fmt.Errorf("provider delegation not found"),
				utils.Attribute{Key: "delegator", Value: delegator},
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chainID", Value: chainID},
			)
			continue
		}
		delegations = append(delegations, delegation)
	}

	return delegations
}

func (k Keeper) UnbondUniformDelegators(ctx sdk.Context, delegator string, amount sdk.Coin) error {
	epoch := k.epochstorageKeeper.GetCurrentNextEpoch(ctx)
	providers, err := k.GetDelegatorProviders(ctx, delegator, epoch)
	_ = err

	// first remove from the empty provider
	if lavaslices.Contains[string](providers, EMPTY_PROVIDER) {
		delegation, found := k.GetDelegation(ctx, delegator, EMPTY_PROVIDER, EMPTY_PROVIDER_CHAINID, epoch)
		if found {
			if delegation.Amount.Amount.GTE(amount.Amount) {
				// we have enough here, remove all from empty delegator and bail
				return k.Unbond(ctx, delegator, EMPTY_PROVIDER, EMPTY_PROVIDER_CHAINID, amount, false)
			} else {
				// we dont have enough in the empty provider, remove everything and continue with the rest
				err = k.Unbond(ctx, delegator, EMPTY_PROVIDER, EMPTY_PROVIDER_CHAINID, delegation.Amount, false)
				if err != nil {
					return err
				}
				amount = amount.Sub(delegation.Amount)
			}
		}
	}

	providers, _ = lavaslices.Remove[string](providers, EMPTY_PROVIDER)
	_ = providers

	var delegations []types.Delegation
	for _, provider := range providers {
		delegations = append(delegations, k.GetAllProviderDelegatorDelegations(ctx, delegator, provider, epoch)...)
	}

	slices.SortFunc(delegations, func(i, j types.Delegation) bool {
		return i.Amount.IsLT(j.Amount)
	})

	delegationLen := int64(len(delegations))
	amountToDeduct := amount.Amount.QuoRaw(delegationLen)
	for _, delegation := range delegations {
		delegationLen--
		if delegation.Amount.Amount.LT(amountToDeduct) {
			err := k.Unbond(ctx, delegation.Delegator, delegation.Provider, delegation.ChainID, delegation.Amount, false) // ?? is it false?
			if err != nil {
				return err
			}
			amountToDeduct = amountToDeduct.Add(amountToDeduct.Sub(delegation.Amount.Amount).QuoRaw(delegationLen))
			amount = amount.Sub(delegation.Amount)
		} else {
			err := k.Unbond(ctx, delegation.Delegator, delegation.Provider, delegation.ChainID, sdk.NewCoin(delegation.Amount.Denom, amountToDeduct), false) // ?? is it false?
			if err != nil {
				return err
			}
			amount = amount.Sub(sdk.NewCoin(delegation.Amount.Denom, amountToDeduct))
		}
	}

	if !amount.IsZero() { // we have leftovers, remove from the highest delegation
		delegation := delegations[len(delegations)-1]
		err := k.Unbond(ctx, delegation.Delegator, delegation.Provider, delegation.ChainID, sdk.NewCoin(delegation.Amount.Denom, amountToDeduct), false) // ?? is it false?
		if err != nil {
			return err
		}
	}
	// [10 20 50 60 70] 25 -> [0 20 50 60 70] 25 + 15/4 -> [0 0 50 60 70] 25 + 15/4 + 8.75/3
	return nil
}

// returns the difference between validators delegations and provider delegation (validators-providers)
func (k Keeper) VerifyDelegatorBalance(ctx sdk.Context, delAddr sdk.AccAddress) (math.Int, error) {
	nextEpoch := k.epochstorageKeeper.GetCurrentNextEpoch(ctx)
	providers, err := k.GetDelegatorProviders(ctx, delAddr.String(), nextEpoch)
	_ = err
	// TODO make this more efficient
	sumProviderDelegations := sdk.ZeroInt()
	for _, p := range providers {
		delegations := k.GetAllProviderDelegatorDelegations(ctx, delAddr.String(), p, nextEpoch)
		for _, d := range delegations {
			sumProviderDelegations = sumProviderDelegations.Add(d.Amount.Amount)
		}
	}

	sumValidatorDelegations := sdk.ZeroInt()
	delegations := k.stakingKeeper.GetAllDelegatorDelegations(ctx, delAddr)
	for _, d := range delegations {
		validatorAddr, err := sdk.ValAddressFromBech32(d.ValidatorAddress)
		if err != nil {
			panic(err) // shouldn't happen
		}
		v, found := k.stakingKeeper.GetValidator(ctx, validatorAddr)
		_ = found
		sumValidatorDelegations = sumValidatorDelegations.Add(v.TokensFromShares(d.Shares).TruncateInt())
	}

	return sumValidatorDelegations.Sub(sumProviderDelegations), nil
}
