// Copyright 2022 TriggerMesh Inc.
// SPDX-License-Identifier: Apache-2.0

package subscriptions

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/rickb777/date/period"
	"go.uber.org/zap"

	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	"knative.dev/eventing/pkg/eventfilter"
	"knative.dev/eventing/pkg/eventfilter/subscriptionsapi"
	"knative.dev/pkg/logging"

	"github.com/triggermesh/brokers/pkg/backend"
	"github.com/triggermesh/brokers/pkg/config"
)

type CloudEventHandler func(context.Context, *cloudevents.Event) error

type Subscription struct {
	Trigger config.Trigger
}

type Manager struct {
	logger   *zap.SugaredLogger
	ceClient cloudevents.Client

	backend   backend.Interface
	ceHandler CloudEventHandler

	triggers    map[string]config.Trigger
	subscribers map[string]*subscriber

	// TODO subs map

	ctx context.Context
	m   sync.RWMutex
}

func New(logger *zap.SugaredLogger, be backend.Interface) (*Manager, error) {
	// Needed for Knative filters
	ctx := context.Background()
	ctx = logging.WithLogger(ctx, logger)

	p, err := cloudevents.NewHTTP()
	if err != nil {
		return nil, fmt.Errorf("could not create CloudEvents HTTP protocol: %w", err)
	}

	ceClient, err := cloudevents.NewClient(p)
	if err != nil {
		return nil, fmt.Errorf("could not create CloudEvents HTTP client: %w", err)
	}

	return &Manager{
		backend:     be,
		subscribers: make(map[string]*subscriber),
		logger:      logger,
		ceClient:    ceClient,
		ctx:         ctx,
	}, nil
}

func (m *Manager) UpdateFromConfig(c *config.Config) {
	m.m.Lock()
	defer m.m.Unlock()

	// if reflect.DeepEqual(m.triggers, c.Triggers) {
	// 	return
	// }

	for k, sub := range m.subscribers {
		if _, ok := c.Triggers[k]; !ok {
			sub.unsubscribe()
			delete(m.subscribers, k)
		}
	}

	for name, trigger := range c.Triggers {
		s, ok := m.subscribers[name]
		if !ok {
			// if not exists create subscription.
			s = &subscriber{
				name:      name,
				backend:   m.backend,
				ceClient:  m.ceClient,
				parentCtx: m.ctx,
				logger:    m.logger,
			}

			if err := s.updateTrigger(trigger); err != nil {
				m.logger.Error("Could not setup trigger", zap.String("trigger", name), zap.Error(err))
				return
			}

			m.backend.Subscribe(name, s.dispatchCloudEvent)
			m.subscribers[name] = s

			continue
		}

		if reflect.DeepEqual(s.trigger, trigger) {
			// no changes for this trigger.
			continue
		}

		// if exists, update data
		m.logger.Info("Updating trigger configuration", zap.String("name", name), zap.Any("trigger", trigger))
		if err := s.updateTrigger(trigger); err != nil {
			m.logger.Error("Could not setup trigger", zap.String("name", name), zap.Error(err))
			return
		}
	}
}

func (m *Manager) DispatchCloudEvent(event *cloudevents.Event) {
	// TODO improve by creating a copy of triggers for this event and
	// avoid locking.
	m.m.RLock()
	defer m.m.RUnlock()

	// var wg sync.WaitGroup
	for i := range m.triggers {
		res := subscriptionsapi.NewAllFilter(materializeFiltersList(m.ctx, m.triggers[i].Filters)...).Filter(m.ctx, *event)
		if res == eventfilter.FailFilter {
			m.logger.Debug("Skipped delivery due to filter", zap.Any("event", *event))
			continue
		}

		// for j := range m.triggers[i].Targets {
		// 	target := &m.triggers[i].Targets[j]
		// 	wg.Add(1)
		// 	go func() {
		// 		defer wg.Done()
		// 		m.dispatchCloudEventToTarget(target, event)
		// 	}()
		// }
		t := m.triggers[i].Target
		m.dispatchCloudEventToTarget(&t, event)
	}
	// wg.Wait()
}

func (m *Manager) RegisterCloudEventHandler(h CloudEventHandler) {
	m.ceHandler = h
}

func (m *Manager) dispatchCloudEventToTarget(target *config.Target, event *cloudevents.Event) {
	ctx := cloudevents.ContextWithTarget(m.ctx, target.URL)

	if target.DeliveryOptions != nil &&
		target.DeliveryOptions.Retry != nil &&
		*target.DeliveryOptions.Retry >= 1 &&
		target.DeliveryOptions.BackoffPolicy != nil {

		delay, err := period.Parse(*target.DeliveryOptions.BackoffDelay)
		if err != nil {
			m.logger.Error(fmt.Sprintf("Event was lost while sending to %s due to backoff delay parsing",
				cloudevents.TargetFromContext(ctx).String()), zap.Bool("lost", true),
				zap.String("type", event.Type()), zap.String("source", event.Source()), zap.String("id", event.ID()), zap.Error(err))
		}

		switch *target.DeliveryOptions.BackoffPolicy {
		case config.BackoffPolicyLinear:
			ctx = cloudevents.ContextWithRetriesLinearBackoff(
				ctx, delay.DurationApprox(), int(*target.DeliveryOptions.Retry))

		case config.BackoffPolicyExponential:
			ctx = cloudevents.ContextWithRetriesExponentialBackoff(
				ctx, delay.DurationApprox(), int(*target.DeliveryOptions.Retry))

		default:
			ctx = cloudevents.ContextWithRetriesConstantBackoff(
				ctx, delay.DurationApprox(), int(*target.DeliveryOptions.Retry))
		}
	}

	if m.send(ctx, event) {
		return
	}

	if target.DeliveryOptions != nil && target.DeliveryOptions.DeadLetterURL != nil &&
		*target.DeliveryOptions.DeadLetterURL != "" {
		ctx = cloudevents.ContextWithTarget(m.ctx, *target.DeliveryOptions.DeadLetterURL)
		if m.send(ctx, event) {
			return
		}
	}

	// Attribute "lost": true is set help log aggregators identify
	// lost events by querying.
	m.logger.Error(fmt.Sprintf("Event was lost while sending to %s",
		cloudevents.TargetFromContext(ctx).String()), zap.Bool("lost", true),
		zap.String("type", event.Type()), zap.String("source", event.Source()), zap.String("id", event.ID()))
}

func (m *Manager) send(ctx context.Context, event *cloudevents.Event) bool {
	res, result := m.ceClient.Request(ctx, *event)

	switch {
	case cloudevents.IsACK(result):
		if res != nil {
			if err := m.ceHandler(ctx, res); err != nil {
				m.logger.Error(fmt.Sprintf("Failed to consume response from %s",
					cloudevents.TargetFromContext(ctx).String()),
					zap.Error(err), zap.String("type", res.Type()), zap.String("source", res.Source()), zap.String("id", res.ID()))

				// Not ingesting the response is considered an error.
				// TODO make this configurable.
				return false
			}
		}
		return true

	case cloudevents.IsUndelivered(result):
		m.logger.Error(fmt.Sprintf("Failed to send event to %s",
			cloudevents.TargetFromContext(ctx).String()),
			zap.Error(result), zap.String("type", event.Type()), zap.String("source", event.Source()), zap.String("id", event.ID()))
		return false

	case cloudevents.IsNACK(result):
		m.logger.Error(fmt.Sprintf("Event not accepted at %s",
			cloudevents.TargetFromContext(ctx).String()),
			zap.Error(result), zap.String("type", event.Type()), zap.String("source", event.Source()), zap.String("id", event.ID()))
		return false
	}

	m.logger.Error(fmt.Sprintf("Unknown event send outcome at %s",
		cloudevents.TargetFromContext(ctx).String()),
		zap.Error(result), zap.String("type", event.Type()), zap.String("source", event.Source()), zap.String("id", event.ID()))
	return false
}

// Copied from Knative Eventing

/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

func materializeFiltersList(ctx context.Context, filters []eventingv1.SubscriptionsAPIFilter) []eventfilter.Filter {
	materializedFilters := make([]eventfilter.Filter, 0, len(filters))
	for _, f := range filters {
		f := materializeSubscriptionsAPIFilter(ctx, f)
		if f == nil {
			logging.FromContext(ctx).Warnw("Failed to parse filter. Skipping filter.", zap.Any("filter", f))
			continue
		}
		materializedFilters = append(materializedFilters, f)
	}
	return materializedFilters
}

func materializeSubscriptionsAPIFilter(ctx context.Context, filter eventingv1.SubscriptionsAPIFilter) eventfilter.Filter {
	var materializedFilter eventfilter.Filter
	var err error
	switch {
	case len(filter.Exact) > 0:
		// The webhook validates that this map has only a single key:value pair.
		for attribute, value := range filter.Exact {
			materializedFilter, err = subscriptionsapi.NewExactFilter(attribute, value)
			if err != nil {
				logging.FromContext(ctx).Debugw("Invalid exact expression", zap.String("attribute", attribute), zap.String("value", value), zap.Error(err))
				return nil
			}
		}
	case len(filter.Prefix) > 0:
		// The webhook validates that this map has only a single key:value pair.
		for attribute, prefix := range filter.Prefix {
			materializedFilter, err = subscriptionsapi.NewPrefixFilter(attribute, prefix)
			if err != nil {
				logging.FromContext(ctx).Debugw("Invalid prefix expression", zap.String("attribute", attribute), zap.String("prefix", prefix), zap.Error(err))
				return nil
			}
		}
	case len(filter.Suffix) > 0:
		// The webhook validates that this map has only a single key:value pair.
		for attribute, suffix := range filter.Suffix {
			materializedFilter, err = subscriptionsapi.NewSuffixFilter(attribute, suffix)
			if err != nil {
				logging.FromContext(ctx).Debugw("Invalid suffix expression", zap.String("attribute", attribute), zap.String("suffix", suffix), zap.Error(err))
				return nil
			}
		}
	case len(filter.All) > 0:
		materializedFilter = subscriptionsapi.NewAllFilter(materializeFiltersList(ctx, filter.All)...)
	case len(filter.Any) > 0:
		materializedFilter = subscriptionsapi.NewAnyFilter(materializeFiltersList(ctx, filter.Any)...)
	case filter.Not != nil:
		materializedFilter = subscriptionsapi.NewNotFilter(materializeSubscriptionsAPIFilter(ctx, *filter.Not))
	case filter.CESQL != "":
		if materializedFilter, err = subscriptionsapi.NewCESQLFilter(filter.CESQL); err != nil {
			// This is weird, CESQL expression should be validated when Trigger's are created.
			logging.FromContext(ctx).Debugw("Found an Invalid CE SQL expression", zap.String("expression", filter.CESQL))
			return nil
		}
	}
	return materializedFilter
}
