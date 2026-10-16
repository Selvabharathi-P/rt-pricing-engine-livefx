package pricingengine

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var symbols = []string{
	"AUDUSD", "AUDCAD", "AUDCHF", "AUDJPY", "AUDNZD", "AUDSGD", "CADCHF", "CADJPY", "CHFJPY", "CHFSGD",
	"EURAUD", "EURCAD", "EURCHF", "EURGBP", "EURJPY", "EURNZD", "EURUSD", "GBPAUD", "GBPCAD", "GBPCHF",
	"GBPJPY", "GBPNZD", "GBPUSD", "NZDCAD", "NZDCHF", "NZDJPY", "NZDUSD", "USDCAD", "USDCHF", "USDCNH",
	"USDJPY", "USDMXN", "USDNOK", "USDPLN", "USDSEK", "USDSGD", "USDTRY", "USDZAR", "EURTRY", "GBPTRY",
	"NOKJPY", "SEKJPY", "SGDJPY", "ZARJPY", "XAUUSD", "XAGUSD", "BTCUSD", "ETHUSD", "BNBUSD", "SOLUSD",
	"XRPUSD", "ADAUSD", "DOGUSD", "DOTUSD", "LTCUSD", "BCHUSD", "XLMUSD", "TRXUSD", "UNIUSD", "FILUSD",
	"AVXUSD",
}

// collect finds every object that has a "symbol" field, whether the message
// is a single tick, an array of ticks, or a wrapper object.
func collect(v interface{}, seen map[string]bool) int {
	n := 0
	switch t := v.(type) {
	case []interface{}:
		for _, x := range t {
			n += collect(x, seen)
		}
	case map[string]interface{}:
		if s, ok := t["symbol"].(string); ok {
			seen[s] = true
			return 1
		}
		for _, x := range t {
			n += collect(x, seen)
		}
	}
	return n
}

func short(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}

func main() {
	url := flag.String("url", "wss://v3-prod.livefxhub.com/ws/algo", "WebSocket endpoint")
	dur := flag.Duration("duration", 15*time.Second, "how long to listen for ticks")
	flag.Parse()

	key, secret := os.Getenv("API_KEY"), os.Getenv("API_SECRET")
	if key == "" || secret == "" {
		log.Fatal("set API_KEY and API_SECRET environment variables")
	}

	// 1. Connect
	c, _, err := websocket.DefaultDialer.Dial(*url, nil)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer c.Close()
	c.SetReadLimit(16 << 20) // feed batches can be ~256KB
	fmt.Println("connected")

	// 2. Authenticate
	err = c.WriteJSON(map[string]string{"action": "auth", "api_key": key, "secret": secret})
	if err != nil {
		log.Fatalf("auth send failed: %v", err)
	}

	// 3. Wait for the auth response (shape is undocumented, so just print it)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := c.ReadMessage()
	if err != nil {
		log.Fatalf("no auth response: %v", err)
	}
	fmt.Println("auth response:", short(msg))
	low := strings.ToLower(string(msg))
	if strings.Contains(low, "error") || strings.Contains(low, "fail") || strings.Contains(low, "unauthor") {
		log.Fatal("authentication appears to have failed")
	}

	// 4. Subscribe
	err = c.WriteJSON(map[string]interface{}{"action": "subscribeMarketPrice", "symbols": symbols})
	if err != nil {
		log.Fatalf("subscribe send failed: %v", err)
	}

	// 5. Verify live ticks
	seen := map[string]bool{}
	msgs, ticks, maxSize := 0, 0, 0
	end := time.Now().Add(*dur)
	for time.Now().Before(end) {
		c.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			log.Fatalf("read failed: %v", err)
		}
		msgs++
		if len(msg) > maxSize {
			maxSize = len(msg)
		}
		if msgs <= 3 {
			fmt.Printf("message #%d: %s\n", msgs, short(msg))
		}
		var v interface{}
		if json.Unmarshal(msg, &v) == nil {
			ticks += collect(v, seen)
		}
	}

	fmt.Printf("\n%d messages, %d ticks, %d/%d symbols seen, largest message %d bytes\n",
		msgs, ticks, len(seen), len(symbols), maxSize)
	if ticks > 0 {
		fmt.Println("OK: receiving live market price updates")
	} else {
		fmt.Println("FAIL: no ticks received (check the raw messages above)")
		os.Exit(1)
	}
}
