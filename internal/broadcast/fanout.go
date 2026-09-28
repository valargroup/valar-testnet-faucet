// Package broadcast pushes a raw transaction to several Zakura nodes directly, so a payout
// reaches every node's mempool without waiting on gossip.
package broadcast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/valargroup/valar-testnet-faucet/internal/jsonrpc"
)

// Node is one named JSON-RPC endpoint.
type Node struct {
	Name string
	URL  string
}

// ParseNodes parses "name=url,name=url" (a bare url is named after itself).
func ParseNodes(spec string) ([]Node, error) {
	var nodes []Node
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, url, ok := strings.Cut(item, "=")
		if !ok {
			name, url = item, item
		}
		name, url = strings.TrimSpace(name), strings.TrimSpace(url)
		if name == "" || !(strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
			return nil, fmt.Errorf("invalid broadcast node %q: want name=http(s)://host:port", item)
		}
		nodes = append(nodes, Node{Name: name, URL: url})
	}
	return nodes, nil
}

// Result is the outcome of broadcasting to one node.
type Result struct {
	Node string
	// OK is true when the node accepted the transaction or already had it.
	OK  bool
	Err error
}

// Fanout broadcasts to a fixed set of nodes.
type Fanout struct {
	nodes   []Node
	clients []*jsonrpc.Client
	timeout time.Duration
}

// New returns a Fanout over nodes, bounding each node's call by timeout.
func New(nodes []Node, timeout time.Duration) *Fanout {
	httpClient := &http.Client{}
	f := &Fanout{nodes: nodes, timeout: timeout}
	for _, n := range nodes {
		f.clients = append(f.clients, jsonrpc.New(n.URL, "", "", httpClient))
	}
	return f
}

// Len returns the number of nodes.
func (f *Fanout) Len() int { return len(f.nodes) }

// Broadcast sends rawHex to every node concurrently and waits for all of them. It never
// fails as a whole: each node's outcome is reported in the returned slice, in node order.
func (f *Fanout) Broadcast(ctx context.Context, rawHex string) []Result {
	results := make([]Result, len(f.nodes))
	var wg sync.WaitGroup
	for i := range f.nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, f.timeout)
			defer cancel()
			err := f.clients[i].Call(callCtx, "sendrawtransaction", []any{rawHex}, nil)
			results[i] = Result{Node: f.nodes[i].Name, OK: err == nil || alreadyKnown(err), Err: err}
		}()
	}
	wg.Wait()
	return results
}

// alreadyKnown reports whether a node refused the transaction only because it already
// has it, in its mempool or in a block. Zebra-family nodes say so in the message rather
// than with a distinct code.
func alreadyKnown(err error) bool {
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		return false
	}
	msg := strings.ToLower(rpcErr.Message)
	return strings.Contains(msg, "already") || strings.Contains(msg, "committed to the best chain")
}
