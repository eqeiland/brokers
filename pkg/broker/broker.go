// Copyright 2022 TriggerMesh Inc.
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/triggermesh/brokers/pkg/backend"
	cfgwatcher "github.com/triggermesh/brokers/pkg/config/watcher"
	"github.com/triggermesh/brokers/pkg/ingest"
	"github.com/triggermesh/brokers/pkg/subscriptions"
	"go.uber.org/zap"
)

type Instance struct {
	backend      backend.Interface
	ingest       *ingest.Instance
	subscription *subscriptions.Manager
	cw           *cfgwatcher.Watcher

	logger *zap.SugaredLogger
}

func NewInstance(backend backend.Interface, ingest *ingest.Instance, subscription *subscriptions.Manager, cw *cfgwatcher.Watcher, logger *zap.SugaredLogger) *Instance {
	return &Instance{
		backend:      backend,
		ingest:       ingest,
		subscription: subscription,
		cw:           cw,

		logger: logger,
	}
}

func (i *Instance) Start(inctx context.Context) error {
	sigctx, stop := signal.NotifyContext(inctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	grp, ctx := errgroup.WithContext(sigctx)

	// Initialization will create structures, execute migrations
	// and claim non processed messages from the backend.
	err := i.backend.Init(ctx)
	if err != nil {
		return fmt.Errorf("could not initialize backend: %v", err)
	}

	// Start is a blocking function that will read messages from the backend
	// implementation and send them to the subscription manager dispatcher.
	// When the dispatcher returns the message is marked as processed.
	grp.Go(func() error {
		return i.backend.Start(ctx)
	})

	// ConfigWatcher will callback reconfigurations for:
	// - Ingest: if authentication parameters are updated.
	// - Subscription manager: if triggers configurations changes.
	i.cw.AddCallback(i.ingest.UpdateFromConfig)
	i.cw.AddCallback(i.subscription.UpdateFromConfig)

	// Start the configuration watcher.
	// There is no need to add it to the wait group
	// since it cleanly exits when context is done.
	if err = i.cw.Start(ctx); err != nil {
		return fmt.Errorf("could not start configuration watcher: %v", err)
	}

	// Register producer function for received events at ingest.
	i.ingest.RegisterCloudEventHandler(i.backend.Produce)

	// TODO register probes at ingest

	// Start the server that ingests CloudEvents.
	grp.Go(func() error {
		err := i.ingest.Start(ctx)
		return err
	})

	return grp.Wait()
}
