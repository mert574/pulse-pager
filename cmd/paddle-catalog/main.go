// Command paddle-catalog fetches the Paddle product/price catalog so we can map
// plans to price ids and read trial periods + custom metadata (RFC-018). The API key
// is read from the environment, never hard-coded or committed; use a read-only
// (products & prices) key.
//
//	export PADDLE_API_KEY=pdl_...        # or PULSE_PADDLE_API_KEY
//	go run ./cmd/paddle-catalog          # human-readable table
//	go run ./cmd/paddle-catalog --json   # raw JSON (prices, product included)
//
// The environment (live vs sandbox) is inferred from the key prefix; set
// PADDLE_API_BASE to override (e.g. https://sandbox-api.paddle.com).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type page struct {
	Data []json.RawMessage `json:"data"`
	Meta struct {
		Pagination struct {
			Next    string `json:"next"`
			HasMore bool   `json:"has_more"`
		} `json:"pagination"`
	} `json:"meta"`
}

type interval struct {
	Interval  string `json:"interval"`
	Frequency int    `json:"frequency"`
}

type price struct {
	ID           string    `json:"id"`
	ProductID    string    `json:"product_id"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	BillingCycle *interval `json:"billing_cycle"`
	TrialPeriod  *interval `json:"trial_period"`
	UnitPrice    struct {
		Amount       string `json:"amount"`
		CurrencyCode string `json:"currency_code"`
	} `json:"unit_price"`
	CustomData json.RawMessage `json:"custom_data"`
	Product    *struct {
		Name string `json:"name"`
	} `json:"product"`
}

func main() {
	key := os.Getenv("PADDLE_API_KEY")
	if key == "" {
		key = os.Getenv("PULSE_PADDLE_API_KEY")
	}
	if key == "" {
		fail("set PADDLE_API_KEY (or PULSE_PADDLE_API_KEY) to a read-only Paddle key")
	}

	base := os.Getenv("PADDLE_API_BASE")
	if base == "" {
		// Sandbox keys carry "sdbx"; everything else is treated as live.
		if strings.Contains(key, "sdbx") {
			base = "https://sandbox-api.paddle.com"
		} else {
			base = "https://api.paddle.com"
		}
	}
	base = strings.TrimRight(base, "/")

	raw, err := fetchPrices(base, key)
	if err != nil {
		fail(err.Error())
	}

	if len(os.Args) > 1 && os.Args[1] == "--json" {
		out, _ := json.MarshalIndent(raw, "", "  ")
		fmt.Println(string(out))
		return
	}

	if len(raw) == 0 {
		fmt.Printf("no active prices found at %s\n", base)
		return
	}

	fmt.Printf("%s  (%d active prices)\n\n", base, len(raw))
	for _, r := range raw {
		var p price
		if err := json.Unmarshal(r, &p); err != nil {
			continue
		}
		name := "?"
		if p.Product != nil {
			name = p.Product.Name
		}
		fmt.Printf("- %s  /  %s\n", name, p.Description)
		fmt.Printf("    price_id:    %s\n", p.ID)
		fmt.Printf("    product_id:  %s\n", p.ProductID)
		fmt.Printf("    price:       %s  (%s)\n", money(p.UnitPrice.Amount, p.UnitPrice.CurrencyCode), cycle(p.BillingCycle))
		fmt.Printf("    trial:       %s\n", trial(p.TrialPeriod))
		if len(p.CustomData) > 0 && string(p.CustomData) != "null" {
			fmt.Printf("    custom_data: %s\n", string(p.CustomData))
		}
		fmt.Println()
	}
}

// fetchPrices returns every active price (with its product) following pagination.
func fetchPrices(base, key string) ([]json.RawMessage, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	url := base + "/prices?include=product&per_page=200&status=active"
	var out []json.RawMessage
	for url != "" {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("could not reach paddle: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("paddle api %d: %s", resp.StatusCode, truncate(string(body), 500))
		}
		var pg page
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, err
		}
		out = append(out, pg.Data...)
		url = ""
		if pg.Meta.Pagination.HasMore {
			url = pg.Meta.Pagination.Next
		}
	}
	return out, nil
}

func money(amount, currency string) string {
	// Paddle amounts are minor units (cents) as a string.
	n, err := strconv.ParseInt(amount, 10, 64)
	if err != nil {
		return amount + " " + currency
	}
	return fmt.Sprintf("%.2f %s", float64(n)/100, currency)
}

func cycle(c *interval) string {
	if c == nil {
		return "one-time"
	}
	return fmt.Sprintf("every %d %s", c.Frequency, c.Interval)
}

func trial(t *interval) string {
	if t == nil {
		return "none"
	}
	return fmt.Sprintf("%d %s", t.Frequency, t.Interval)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "paddle-catalog:", msg)
	os.Exit(1)
}
