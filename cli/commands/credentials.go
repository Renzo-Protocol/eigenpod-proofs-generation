package commands

import (
	"context"
	"fmt"
	"math"
	"math/big"

	"github.com/Layr-Labs/eigenpod-proofs-generation/cli/core"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/fatih/color"
)

type TCredentialCommandArgs struct {
	EigenpodAddress string

	DisableColor        bool
	UseJSON             bool
	SimulateTransaction bool
	Node                string
	BeaconNode          string
	Sender              string
	SpecificValidator   uint64
	BatchSize           uint64
	NoPrompt            bool
	Verbose             bool
}

func CredentialsCommand(args TCredentialCommandArgs) error {
	ctx := context.Background()
	if args.DisableColor {
		color.NoColor = true
	}

	isVerbose := (!args.UseJSON && !args.SimulateTransaction) || args.Verbose

	eth, beaconClient, chainId, err := core.GetClients(ctx, args.Node, args.BeaconNode, isVerbose)
	core.PanicOnError("failed to reach ethereum clients", err)

	if args.SimulateTransaction && len(args.Sender) > 0 {
		core.Panic("if using --print-calldata, please do not specify a --sender.")
		return nil
	}

	var specificValidatorIndex *big.Int = nil
	if args.SpecificValidator != math.MaxUint64 && args.SpecificValidator != 0 {
		specificValidatorIndex = new(big.Int).SetUint64(args.SpecificValidator)
		if isVerbose {
			fmt.Printf("Using specific validator: %d", args.SpecificValidator)
		}
	}

	validatorProofs, oracleBeaconTimestamp, err := core.GenerateValidatorProof(ctx, args.EigenpodAddress, eth, chainId, beaconClient, specificValidatorIndex, isVerbose)

	if err != nil || validatorProofs == nil {
		core.PanicOnError("Failed to generate validator proof", err)
		core.Panic("no inactive validators")
	}

	if len(args.Sender) != 0 || args.SimulateTransaction {
		txns, indices, err := core.SubmitValidatorProof(ctx, args.Sender, args.EigenpodAddress, chainId, eth, args.BatchSize, validatorProofs, oracleBeaconTimestamp, args.NoPrompt, args.SimulateTransaction, isVerbose)
		core.PanicOnError(fmt.Sprintf("failed to %s validator proof", func() string {
			if args.SimulateTransaction {
				return "simulate"
			} else {
				return "submit"
			}
		}()), err)

		if args.SimulateTransaction {
			out := aMap(txns, func(txn *types.Transaction) CredentialProofTransaction {
				return CredentialProofTransaction{
					Transaction: Transaction{
						Type:     "credential_proof",
						To:       txn.To().Hex(),
						CallData: common.Bytes2Hex(txn.Data()),
					},
					ValidatorIndices: aMap(aFlatten(indices), func(index *big.Int) uint64 {
						return index.Uint64()
					}),
				}
			})
			printProofs(out)
		} else {
			for i, txn := range txns {
				color.Green("transaction(%d): %s", i, txn.Hash().Hex())
			}
		}

		core.PanicOnError("failed to invoke verifyWithdrawalCredentials", err)
	}
	return nil
}
