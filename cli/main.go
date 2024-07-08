package main

import (
	"bytes"
	sha256 "crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"time"

	"context"

	"github.com/Layr-Labs/eigenpod-proofs-generation/cli/onchain"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	cli "github.com/urfave/cli/v2"
)

func shortenHex(publicKey string) string {
	return publicKey[0:6] + ".." + publicKey[len(publicKey)-4:]
}

func main() {
	var eigenpodAddress, beacon, node, owner, output string
	var forceCheckpoint, disableColor, verbose bool
	var useJson bool = false
	ctx := context.Background()

	app := &cli.App{
		Name:                   "Eigenlayer Proofs CLi",
		HelpName:               "eigenproofs",
		Usage:                  "Generates proofs to (1) checkpoint your validators, or (2) verify the withdrawal credentials of an inactive validator.",
		EnableBashCompletion:   true,
		UseShortOptionHandling: true,
		Commands: []*cli.Command{
			{
				Name:  "status",
				Usage: "Checks the status of your eigenpod.",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:        "json",
						Value:       false,
						Usage:       "print only plain JSON",
						Required:    false,
						Destination: &useJson,
					},
				},
				Action: func(cctx *cli.Context) error {
					if disableColor {
						color.NoColor = true
					}

					eth, err := ethclient.Dial(node)
					PanicOnError("failed to reach eth --node.", err)

					beaconClient, err := getBeaconClient(beacon)
					PanicOnError("failed to reach beacon chain.", err)

					status := getStatus(ctx, eigenpodAddress, eth, beaconClient)

					if useJson {
						bytes, err := json.MarshalIndent(status, "", "      ")
						PanicOnError("failed to get status", err)
						statusStr := string(bytes)
						fmt.Println(statusStr)
					} else {
						// pretty print everything
						color.New(color.Bold, color.FgBlue).Printf("Eigenpod validators\n")
						for index, validator := range status.Validators {

							var targetColor color.Attribute
							var description string

							if validator.Status == ValidatorStatusActive {
								targetColor = color.FgGreen
								description = "active"
							} else if validator.Status == ValidatorStatusInactive {
								targetColor = color.FgHiYellow
								description = "inactive"
							} else if validator.Status == ValidatorStatusWithdrawn {
								targetColor = color.FgHiRed
								description = "withdrawn"
							}

							if validator.Slashed {
								description = description + " (slashed)"
							}

							publicKey := validator.PublicKey
							if !verbose {
								publicKey = shortenHex(publicKey)
							}

							color.New(targetColor).Printf("\t- #%s (%s) [%s]\n", index, publicKey, description)
						}

						bold := color.New(color.Bold, color.FgBlue)
						ital := color.New(color.Italic, color.FgBlue)
						fmt.Println()

						if status.ActiveCheckpoint != nil {
							startTime := time.Unix(int64(status.ActiveCheckpoint.StartedAt), 0)

							bold.Printf("!NOTE: There is a checkpoint active! (started at: %s)\n", startTime.String())

							endSharesETH := gweiToEther(status.ActiveCheckpoint.PendingSharesGwei)
							deltaETH := new(big.Float).Sub(
								endSharesETH,
								status.CurrentTotalSharesETH,
							) // delta = endShares - currentOwnerSharesETH

							ital.Printf("\t- If you finish it, you may receive up to %s shares. (%s -> %s)\n", deltaETH.String(), status.CurrentTotalSharesETH.String(), endSharesETH.String())

							ital.Printf("\t- %d proof(s) remaining until completion.\n", status.ActiveCheckpoint.ProofsRemaining)
						} else {
							bold.Printf("Runing a `checkpoint` right now will result in: \n")

							startEther := status.CurrentTotalSharesETH
							endEther := status.TotalSharesAfterCheckpointETH
							delta := new(big.Float).Sub(endEther, startEther)

							ital.Printf("\t%f new shares issued (%f ==> %f)\n", delta, startEther, endEther)
						}

					}
					return nil
				},
			},
			{
				Name:    "checkpoint",
				Aliases: []string{"cp"},
				Usage:   "Generates a proof for use with EigenPod.verifyCheckpointProofs().",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:        "force",
						Aliases:     []string{"f"},
						Value:       false,
						Usage:       "If true, starts a checkpoint even if the pod has no native ETH to award shares",
						Destination: &forceCheckpoint,
					},
				},
				Action: func(cctx *cli.Context) error {
					if disableColor {
						color.NoColor = true
					}

					var out, owner *string = nil, nil

					if len(cctx.String("out")) > 0 {
						outProp := cctx.String("out")
						out = &outProp
					}

					if len(cctx.String("owner")) > 0 {
						ownerProp := cctx.String("owner")
						owner = &ownerProp
					}

					eth, beaconClient, chainId := getClients(ctx, node, beacon)

					currentCheckpoint := getCurrentCheckpoint(eigenpodAddress, eth)
					if currentCheckpoint == 0 {
						if owner != nil {
							newCheckpoint, err := startCheckpoint(ctx, eigenpodAddress, *owner, chainId, eth, forceCheckpoint)
							PanicOnError("failed to start checkpoint", err)
							currentCheckpoint = newCheckpoint
						} else {
							PanicOnError("no checkpoint active and no private key provided to start one", errors.New("no checkpoint"))
						}
					}
					color.Green("pod has active checkpoint! checkpoint timestamp: %d", currentCheckpoint)

					proof := GenerateCheckpointProof(ctx, eigenpodAddress, eth, chainId, beaconClient)

					jsonString, err := json.Marshal(proof)
					PanicOnError("failed to generate JSON proof data.", err)

					WriteOutputToFileOrStdout(jsonString, out)

					if owner != nil {
						// submit the proof onchain
						ownerAccount, err := prepareAccount(owner, chainId)
						PanicOnError("failed to parse private key", err)

						eigenPod, err := onchain.NewEigenPod(common.HexToAddress(eigenpodAddress), eth)
						PanicOnError("failed to reach eigenpod", err)

						color.Green("calling EigenPod.VerifyCheckpointProofs()...")

						txn, err := eigenPod.VerifyCheckpointProofs(
							ownerAccount.TransactionOptions,
							onchain.BeaconChainProofsBalanceContainerProof{
								BalanceContainerRoot: proof.ValidatorBalancesRootProof.ValidatorBalancesRoot,
								Proof:                proof.ValidatorBalancesRootProof.Proof.ToByteSlice(),
							},
							castBalanceProofs(proof.BalanceProofs),
						)

						PanicOnError("failed to invoke verifyCheckpointProofs", err)
						color.Green("transaction: %s", txn.Hash().Hex())
					}

					return nil
				},
			},
			{
				Name:    "credentials",
				Aliases: []string{"cr", "creds"},
				Usage:   "Generates a proof for use with EigenPod.verifyWithdrawalCredentials()",
				Action: func(cctx *cli.Context) error {
					if disableColor {
						color.NoColor = true
					}

					var out, owner *string = nil, nil

					if len(cctx.String("out")) > 0 {
						outProp := cctx.String("out")
						out = &outProp
					}

					if len(cctx.String("owner")) > 0 {
						ownerProp := cctx.String("owner")
						owner = &ownerProp
					}

					eth, beaconClient, chainId := getClients(ctx, node, beacon)
					validatorProofs, validatorIndices := GenerateValidatorProof(ctx, eigenpodAddress, eth, chainId, beaconClient)
					if validatorProofs == nil || validatorIndices == nil {
						return nil
					}

					jsonString, err := json.Marshal(validatorProofs)
					PanicOnError("failed to generate JSON proof data.", err)

					WriteOutputToFileOrStdout(jsonString, out)

					if owner != nil {
						ownerAccount, err := prepareAccount(owner, chainId)
						PanicOnError("failed to parse private key", err)

						eigenPod, err := onchain.NewEigenPod(common.HexToAddress(eigenpodAddress), eth)
						PanicOnError("failed to reach eigenpod", err)

						indices := Uint64ArrayToBigIntArray(validatorIndices)

						var validatorFieldsProofs [][]byte = [][]byte{}
						for i := 0; i < len(validatorProofs.ValidatorFieldsProofs); i++ {
							pr := validatorProofs.ValidatorFieldsProofs[i].ToByteSlice()
							validatorFieldsProofs = append(validatorFieldsProofs, pr)
						}

						var validatorFields [][][32]byte = castValidatorFields(validatorProofs.ValidatorFields)

						latestBlock, err := eth.BlockByNumber(ctx, nil)
						PanicOnError("failed to load latest block", err)

						color.Green("submitting onchain...")
						txn, err := eigenPod.VerifyWithdrawalCredentials(
							ownerAccount.TransactionOptions,
							latestBlock.Time(),
							onchain.BeaconChainProofsStateRootProof{
								Proof:           validatorProofs.StateRootProof.Proof.ToByteSlice(),
								BeaconStateRoot: validatorProofs.StateRootProof.BeaconStateRoot,
							},
							indices,
							validatorFieldsProofs,
							validatorFields,
						)

						PanicOnError("failed to invoke verifyWithdrawalCredentials", err)

						color.Green("transaction: %s", txn.Hash().Hex())
					}
					return nil
				},
			},
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "podAddress",
				Aliases:     []string{"p", "pod"},
				Value:       "",
				Usage:       "[required] The onchain `address` of your eigenpod contract (0x123123123123)",
				Required:    true,
				Destination: &eigenpodAddress,
			},
			&cli.StringFlag{
				Name:        "beaconNode",
				Aliases:     []string{"b"},
				Value:       "",
				Usage:       "[required] `URL` to a functioning beacon node RPC (https://)",
				Required:    true,
				Destination: &beacon,
			},
			&cli.StringFlag{
				Name:        "execNode",
				Aliases:     []string{"e"},
				Value:       "",
				Usage:       "[required] `URL` to a functioning execution-layer RPC (https://)",
				Required:    true,
				Destination: &node,
			},
			&cli.StringFlag{
				Name:        "out",
				Aliases:     []string{"O", "output"},
				Value:       "",
				Usage:       "Output `path` for the proof. (defaults to stdout)",
				Destination: &output,
			},
			&cli.StringFlag{
				Name:        "owner",
				Aliases:     []string{"o"},
				Value:       "",
				Usage:       "`Private key` of the owner. If set, this will automatically submit the proofs to their corresponding onchain functions after generation. If using checkpoint mode, it will also begin a checkpoint if one hasn't been started already.",
				Destination: &owner,
			},
			&cli.BoolFlag{
				Name:        "no-color",
				Value:       false,
				Usage:       "Disables color output for terminals that do not support ANSI color codes.",
				Destination: &disableColor,
			},
			&cli.BoolFlag{
				Name:        "verbose",
				Aliases:     []string{"v"},
				Value:       false,
				Usage:       "Enable verbose output.",
				Destination: &verbose,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		panic(err)
	}
}

func getBeaconClient(beaconUri string) (BeaconClient, error) {
	beaconClient, _, err := NewBeaconClient(beaconUri)
	return beaconClient, err
}

func getCurrentCheckpoint(eigenpodAddress string, client *ethclient.Client) uint64 {
	eigenPod, err := onchain.NewEigenPod(common.HexToAddress(eigenpodAddress), client)
	PanicOnError("failed to locate eigenpod. is your address correct?", err)

	timestamp, err := eigenPod.CurrentCheckpointTimestamp(nil)
	PanicOnError("failed to locate eigenpod. Is your address correct?", err)

	return timestamp
}

// search through beacon state for validators whose withdrawal address is set to eigenpod.
func findAllValidatorsForEigenpod(eigenpodAddress string, beaconState *spec.VersionedBeaconState) []ValidatorWithIndex {
	allValidators, err := beaconState.Validators()
	PanicOnError("failed to fetch beacon state", err)

	eigenpodAddressBytes := common.FromHex(eigenpodAddress)

	var outputValidators []ValidatorWithIndex = []ValidatorWithIndex{}
	var i uint64 = 0
	maxValidators := uint64(len(allValidators))
	for i = 0; i < maxValidators; i++ {
		validator := allValidators[i]
		if validator == nil || validator.WithdrawalCredentials[0] != 1 { // withdrawalCredentials _need_ their first byte set to 1 to withdraw to execution layer.
			continue
		}
		// we check that the last 20 bytes of expectedCredentials matches validatorCredentials.
		if bytes.Equal(
			eigenpodAddressBytes[:],
			validator.WithdrawalCredentials[12:], // first 12 bytes are not the pubKeyHash, see (https://github.com/Layr-Labs/eigenlayer-contracts/blob/d148952a2942a97a218a2ab70f9b9f1792796081/src/contracts/pods/EigenPod.sol#L663)
		) {
			outputValidators = append(outputValidators, ValidatorWithIndex{
				Validator: validator,
				Index:     i,
			})
		}
	}
	return outputValidators
}

func getOnchainValidatorInfo(client *ethclient.Client, eigenpodAddress string, allValidators []ValidatorWithIndex) []onchain.IEigenPodValidatorInfo {
	eigenPod, err := onchain.NewEigenPod(common.HexToAddress(eigenpodAddress), client)
	PanicOnError("failed to locate Eigenpod. Is your address correct?", err)

	var validatorInfo []onchain.IEigenPodValidatorInfo = []onchain.IEigenPodValidatorInfo{}

	// TODO: batch/multicall
	zeroes := [16]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i := 0; i < len(allValidators); i++ {
		// ssz requires values to be 32-byte aligned, which requires 16 bytes of 0's to be added
		// prior to hashing.
		pubKeyHash := sha256.Sum256(
			append(
				(allValidators[i]).Validator.PublicKey[:],
				zeroes[:]...,
			),
		)
		info, err := eigenPod.ValidatorPubkeyHashToInfo(nil, pubKeyHash)
		PanicOnError("failed to fetch validator eigeninfo.", err)
		validatorInfo = append(validatorInfo, info)
	}

	return validatorInfo
}

func getCurrentCheckpointBlockRoot(eigenpodAddress string, eth *ethclient.Client) (*[32]byte, error) {
	eigenPod, err := onchain.NewEigenPod(common.HexToAddress(eigenpodAddress), eth)
	PanicOnError("failed to locate Eigenpod. Is your address correct?", err)

	checkpoint, err := eigenPod.CurrentCheckpoint(nil)
	PanicOnError("failed to reach eigenpod.", err)

	return &checkpoint.BeaconBlockRoot, nil
}

func getClients(ctx context.Context, node, beaconNodeUri string) (*ethclient.Client, BeaconClient, *big.Int) {
	eth, err := ethclient.Dial(node)
	PanicOnError("failed to reach eth --node.", err)

	chainId, err := eth.ChainID(ctx)
	PanicOnError("failed to fetch chain id", err)

	beaconClient, err := getBeaconClient(beaconNodeUri)
	PanicOnError("failed to reach beacon chain.", err)

	return eth, beaconClient, chainId
}
