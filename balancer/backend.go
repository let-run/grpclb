package balancer

import (
	"strings"
	"sync/atomic"
	"time"

	backendpb "github.com/bsm/grpclb/grpclb_backend_v1"
	balancerpb "github.com/bsm/grpclb/grpclb_balancer_v1"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"
)

type backend struct {
	cc  *grpc.ClientConn
	cln backendpb.LoadReportClient

	target  string
	address string
	score   int64

	maxFailures int
	failures    int
}

func newBackend(target, address string, maxFailures int) (*backend, error) {
	var err error

	b := &backend{
		target:  target,
		address: address,

		maxFailures: maxFailures,
	}

	ctx, _ := context.WithTimeout(context.Background(), 2*time.Second)
	b.cc, err = grpc.DialContext(ctx, address, grpc.WithInsecure(), grpc.WithBlock())
	grpclog.Info("connect to:", address)
	if err != nil {
		grpclog.Infof("can't connect to load reporter: %v", err)
		return b, nil
	}

	b.cln = backendpb.NewLoadReportClient(b.cc)

	if err := b.UpdateScore(); err != nil {
		b.Close()
		return nil, err
	}

	return b, nil
}

func (b *backend) Server() *balancerpb.Server {
	return &balancerpb.Server{
		Address: b.address,
		Score:   b.Score(),
	}
}

func (b *backend) Score() int64 {
	return atomic.LoadInt64(&b.score)
}

func (b *backend) UpdateScore() error {
	if b.cln == nil {
		return nil
	}

	resp, err := b.cln.Load(context.Background(), &backendpb.LoadRequest{})
	if err != nil {
		return b.handleError(err)
	}
	b.failures = 0 // clear failures on success
	atomic.StoreInt64(&b.score, resp.Score)
	return nil
}

func (b *backend) Close() error {
	if b.cc == nil {
		return nil
	}

	return b.cc.Close()
}

func (b *backend) handleError(err error) error {
	switch err {
	case grpc.ErrClientConnClosing:
		return err
	}

	code := grpc.Code(err)
	if code == codes.Unimplemented {
		return nil
	}

	b.failures++
	grpclog.Printf("error retrieving load score for %s from %s (failures: %d): %s", b.target, b.address, b.failures, err)

	// recoverable errors:
	switch code {
	case codes.Canceled:
		if strings.Contains(err.Error(), "closing") {
			return err
		}
		fallthrough
	case codes.DeadlineExceeded,
		codes.ResourceExhausted,
		codes.FailedPrecondition,
		codes.Aborted:

		if b.maxFailures > 0 && b.failures >= b.maxFailures {
			return err
		}
		return nil
	}

	// fatal errors:
	return err
}
