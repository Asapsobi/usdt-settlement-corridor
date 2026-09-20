// Run this YOURSELF, in your own terminal:
//
//	go run . -expect-address=T...
//
// -expect-address is the TRON wallet you're sweeping -- this tool
// refuses to sign anything if the private key you paste in doesn't
// actually control that address. It asks for your raw hex private key
// (typed here, never sent anywhere), verifies the derived address
// matches, reads the wallet's own live USDT-TRC20 balance, then builds,
// signs, and broadcasts a real transfer of that full balance to a
// destination you choose. Reuses this module's own real, verified
// transaction-building and broadcast code (internal/txbuild,
// internal/dispatch's GrpcBroadcastClient) rather than a second,
// independently-written TRON client -- same reasoning as
// docs/03-build's own "no second RPC client" convention elsewhere in
// this project. Nothing here ever sends your private key anywhere --
// it only ever leaves this process as a signed transaction, and only
// after you've confirmed the details printed below.
//
// Does NOT sweep TRX: a TRC20 transfer's own energy cost is expected to
// be covered by a delegation already rented for this address (e.g. via
// energybroker/cmd/rent-energy), not by burning this wallet's own TRX,
// so there's normally nothing meaningful left to sweep there.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/fbsobreira/gotron-sdk/pkg/signer"
	"google.golang.org/protobuf/proto"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/money"
	"dispatcher/internal/txbuild"
)

const usdtContractAddress = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
}

func run() error {
	expectAddress := flag.String("expect-address", "", "the TRON address this private key must control (required -- refuses to sign if it doesn't match)")
	tronAPIBaseURL := flag.String("tron-api-base-url", "https://api.trongrid.io", "TRON HTTP API, for reading the live USDT balance")
	tronGRPCAddr := flag.String("tron-grpc-addr", "grpc.trongrid.io:50051", "TRON gRPC node, for the block reference and broadcast")
	flag.Parse()

	if *expectAddress == "" {
		return fmt.Errorf("-expect-address is required")
	}

	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Destination TRON address to sweep the USDT to: ")
	destLine, _ := reader.ReadString('\n')
	dest := strings.TrimSpace(destLine)
	if dest == "" {
		return fmt.Errorf("a destination address is required")
	}

	fmt.Print("Paste the private key for " + *expectAddress + " (hex, no 0x prefix), then press Enter: ")
	keyLine, _ := reader.ReadString('\n')
	keyHex := strings.TrimSpace(keyLine)

	ecdsaKey, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return fmt.Errorf("parsing private key: %w", err)
	}

	sign, err := signer.NewPrivateKeySigner(ecdsaKey)
	if err != nil {
		return fmt.Errorf("building signer: %w", err)
	}
	senderAddress := sign.Address().String()
	if senderAddress != *expectAddress {
		return fmt.Errorf("this private key controls %s, NOT the expected %s -- wrong key, stopping before signing anything",
			senderAddress, *expectAddress)
	}
	fmt.Println("Derived address matches the expected wallet. Proceeding.")

	ctx := context.Background()

	balance, err := usdtBalance(ctx, *tronAPIBaseURL, senderAddress)
	if err != nil {
		return fmt.Errorf("checking USDT balance: %w", err)
	}
	fmt.Printf("Current USDT balance at %s: %s\n", senderAddress, balance.Format())
	if balance <= 0 {
		return fmt.Errorf("balance is zero -- nothing to sweep")
	}

	client, err := dispatch.NewGrpcBroadcastClient(*tronGRPCAddr, 15*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to TRON node: %w", err)
	}
	ref, err := client.CurrentBlockReference(ctx)
	if err != nil {
		return fmt.Errorf("fetching current block reference: %w", err)
	}

	rawBytes, err := txbuild.BuildTransfer(senderAddress, dest, balance, ref)
	if err != nil {
		return fmt.Errorf("building transaction: %w", err)
	}
	var raw core.TransactionRaw
	if err := proto.Unmarshal(rawBytes, &raw); err != nil {
		return fmt.Errorf("internal error: re-parsing built transaction: %w", err)
	}
	tx := &core.Transaction{RawData: &raw}

	signedTx, err := sign.Sign(tx)
	if err != nil {
		return fmt.Errorf("signing transaction: %w", err)
	}

	fmt.Printf("About to send %s USDT from %s to %s.\n", balance.Format(), senderAddress, dest)
	fmt.Print("Type 'yes' to broadcast this real transaction, anything else to abort: ")
	confirmLine, _ := reader.ReadString('\n')
	if strings.TrimSpace(confirmLine) != "yes" {
		return fmt.Errorf("aborted, nothing was sent")
	}

	txid, err := client.Broadcast(ctx, signedTx)
	if err != nil {
		return fmt.Errorf("broadcasting transaction: %w", err)
	}
	fmt.Println("Sent. Transaction id:", txid)
	fmt.Println("Track it at: https://tronscan.org/#/transaction/" + txid)
	return nil
}

// usdtBalance reads address's live USDT-TRC20 balance directly off
// TronGrid's own REST account endpoint -- a plain informational read
// (this tool's only non-signing network call besides the block
// reference/broadcast), same contract address txbuild's own
// USDTContractAddress constant names.
func usdtBalance(ctx context.Context, apiBaseURL, address string) (money.Amount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiBaseURL, "/")+"/v1/accounts/"+address, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var payload struct {
		Data []struct {
			Trc20 []map[string]string `json:"trc20"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("decoding TronGrid response: %w", err)
	}
	if len(payload.Data) == 0 {
		return 0, fmt.Errorf("TronGrid returned no account data for %s", address)
	}
	for _, entry := range payload.Data[0].Trc20 {
		if raw, ok := entry[usdtContractAddress]; ok {
			units, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing USDT balance %q: %w", raw, err)
			}
			return money.Amount(units), nil
		}
	}
	return 0, nil // no USDT entry at all means a zero balance, not an error
}
