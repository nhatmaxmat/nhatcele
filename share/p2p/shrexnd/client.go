package shrexnd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	"github.com/celestiaorg/go-libp2p-messenger/serde"
	"github.com/celestiaorg/nmt"
	"github.com/celestiaorg/nmt/namespace"

	"github.com/celestiaorg/celestia-node/share"
	"github.com/celestiaorg/celestia-node/share/ipld"
	"github.com/celestiaorg/celestia-node/share/p2p"
	pb "github.com/celestiaorg/celestia-node/share/p2p/shrexnd/pb"
)

// Client implements client side of shrex/nd protocol to obtain namespaced shares data from remote
// peers.
type Client struct {
	params     *Parameters
	protocolID protocol.ID

	host    host.Host
	metrics *p2p.Metrics
}

// NewClient creates a new shrEx/nd client
func NewClient(params *Parameters, host host.Host) (*Client, error) {
	if err := params.Validate(); err != nil {
		return nil, fmt.Errorf("shrex-nd: client creation failed: %w", err)
	}

	return &Client{
		host:       host,
		protocolID: p2p.ProtocolID(params.NetworkID(), protocolString),
		params:     params,
	}, nil
}

// RequestND requests namespaced data from the given peer.
// Returns valid data with its verified inclusion against the share.Root.
func (c *Client) RequestND(
	ctx context.Context,
	root *share.Root,
	nID namespace.ID,
	peer peer.ID,
) (share.NamespacedShares, error) {
	shares, err := c.doRequest(ctx, root, nID, peer)
	if err == nil {
		return shares, err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		c.metrics.ObserveRequests(ctx, 1, p2p.StatusTimeout)
		return nil, err
	}
	// some net.Errors also mean the context deadline was exceeded, but yamux/mocknet do not
	// unwrap to a ctx err
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		if deadline, _ := ctx.Deadline(); deadline.Before(time.Now()) {
			c.metrics.ObserveRequests(ctx, 1, p2p.StatusTimeout)
			return nil, context.DeadlineExceeded
		}
	}
	if err != p2p.ErrNotFound && err != share.ErrNamespaceNotFound {
		log.Warnw("client-nd: peer returned err", "err", err)
	}
	return nil, err
}

func (c *Client) doRequest(
	ctx context.Context,
	root *share.Root,
	nID namespace.ID,
	peerID peer.ID,
) (share.NamespacedShares, error) {
	stream, err := c.host.NewStream(ctx, peerID, c.protocolID)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	c.setStreamDeadlines(ctx, stream)

	req := &pb.GetSharesByNamespaceRequest{
		RootHash:    root.Hash(),
		NamespaceId: nID,
	}

	_, err = serde.Write(stream, req)
	if err != nil {
		stream.Reset() //nolint:errcheck
		return nil, fmt.Errorf("client-nd: writing request: %w", err)
	}

	err = stream.CloseWrite()
	if err != nil {
		log.Debugw("client-nd: closing write side of the stream", "err", err)
	}

	var resp pb.GetSharesByNamespaceResponse
	_, err = serde.Read(stream, &resp)
	if err != nil {
		// server is overloaded and closed the stream
		if errors.Is(err, io.EOF) {
			c.metrics.ObserveRequests(ctx, 1, p2p.StatusRateLimited)
			return nil, p2p.ErrNotFound
		}
		stream.Reset() //nolint:errcheck
		return nil, fmt.Errorf("client-nd: reading response: %w", err)
	}

	if err = c.statusToErr(ctx, resp.Status); err != nil {
		return nil, fmt.Errorf("client-nd: response code is not OK: %w", err)
	}

	shares, err := convertToNamespacedShares(resp.Rows)
	if err != nil {
		return nil, fmt.Errorf("client-nd: converting response to shares: %w", err)
	}
	return shares, nil
}

// convertToNamespacedShares converts proto Rows to share.NamespacedShares
func convertToNamespacedShares(rows []*pb.Row) (share.NamespacedShares, error) {
	shares := make([]share.NamespacedRow, 0, len(rows))
	for _, row := range rows {
		var proof *nmt.Proof
		if row.Proof != nil {
			tmpProof := nmt.NewInclusionProof(
				int(row.Proof.Start),
				int(row.Proof.End),
				row.Proof.Nodes,
				ipld.NMTIgnoreMaxNamespace,
			)
			proof = &tmpProof
		}

		shares = append(shares, share.NamespacedRow{
			Shares: row.Shares,
			Proof:  proof,
		})
	}
	return shares, nil
}

func (c *Client) setStreamDeadlines(ctx context.Context, stream network.Stream) {
	// set read/write deadline to use context deadline if it exists
	deadline, ok := ctx.Deadline()
	if ok {
		err := stream.SetDeadline(deadline)
		if err == nil {
			return
		}
		log.Debugw("client-nd: set stream deadline", "err", err)
	}

	// if deadline not set, client read deadline defaults to server write deadline
	if c.params.ServerWriteTimeout != 0 {
		err := stream.SetReadDeadline(time.Now().Add(c.params.ServerWriteTimeout))
		if err != nil {
			log.Debugw("client-nd: set read deadline", "err", err)
		}
	}

	// if deadline not set, client write deadline defaults to server read deadline
	if c.params.ServerReadTimeout != 0 {
		err := stream.SetWriteDeadline(time.Now().Add(c.params.ServerReadTimeout))
		if err != nil {
			log.Debugw("client-nd: set write deadline", "err", err)
		}
	}
}

func (c *Client) statusToErr(ctx context.Context, code pb.StatusCode) error {
	switch code {
	case pb.StatusCode_OK:
		c.metrics.ObserveRequests(ctx, 1, p2p.StatusSuccess)
		return nil
	case pb.StatusCode_NOT_FOUND:
		c.metrics.ObserveRequests(ctx, 1, p2p.StatusNotFound)
		return p2p.ErrNotFound
	case pb.StatusCode_NAMESPACE_NOT_FOUND:
		return share.ErrNamespaceNotFound
	case pb.StatusCode_INVALID:
		log.Debug("client-nd: invalid request")
		fallthrough
	case pb.StatusCode_INTERNAL:
		fallthrough
	default:
		return p2p.ErrInvalidResponse
	}
}
