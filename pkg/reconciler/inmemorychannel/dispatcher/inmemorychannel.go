/*
Copyright 2019 The Knative Authors

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

package dispatcher

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"knative.dev/pkg/apis/duck"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/reconciler"

	eventingduckv1 "knative.dev/eventing/pkg/apis/duck/v1"
	v1 "knative.dev/eventing/pkg/apis/messaging/v1"
	"knative.dev/eventing/pkg/channel"
	"knative.dev/eventing/pkg/channel/fanout"
	"knative.dev/eventing/pkg/channel/multichannelfanout"
	messagingv1 "knative.dev/eventing/pkg/client/clientset/versioned/typed/messaging/v1"
	reconcilerv1 "knative.dev/eventing/pkg/client/injection/reconciler/messaging/v1/inmemorychannel"
	"knative.dev/eventing/pkg/kncloudevents"
)

// Reconciler reconciles InMemory Channels.
type Reconciler struct {
	multiChannelMessageHandler multichannelfanout.MultiChannelMessageHandler
	reporter                   channel.StatsReporter
	messagingClientSet         messagingv1.MessagingV1Interface
}

// Check the interfaces Reconciler should implement
var (
	_ reconcilerv1.Interface         = (*Reconciler)(nil)
	_ reconcilerv1.ReadOnlyInterface = (*Reconciler)(nil)
)

// ReconcileKind implements inmemorychannel.Interface.
func (r *Reconciler) ReconcileKind(ctx context.Context, imc *v1.InMemoryChannel) reconciler.Event {
	if err := r.reconcile(ctx, imc); err != nil {
		return err
	}

	// Then patch the subscribers to reflect that they are now ready to go
	return r.patchSubscriberStatus(ctx, imc)
}

// ObserveKind implements inmemorychannel.ReadOnlyInterface.
func (r *Reconciler) ObserveKind(ctx context.Context, imc *v1.InMemoryChannel) reconciler.Event {
	return r.reconcile(ctx, imc)
}

func (r *Reconciler) reconcile(ctx context.Context, imc *v1.InMemoryChannel) reconciler.Event {
	logging.FromContext(ctx).Infow("Reconciling", zap.Any("InMemoryChannel", imc))

	if !imc.IsReady() {
		logging.FromContext(ctx).Debug("IMC is not ready, skipping")
		return nil
	}

	config, err := newConfigForInMemoryChannel(imc)
	if err != nil {
		logging.FromContext(ctx).Error("Error creating config for in memory channels", zap.Error(err))
		return err
	}

	// First grab the host based MultiChannelFanoutMessage httpHandler
	httpHandler := r.multiChannelMessageHandler.GetChannelHandler(config.HostName)
	if httpHandler == nil {
		// No handler yet, create one.
		fanoutHandler, err := fanout.NewFanoutMessageHandler(
			logging.FromContext(ctx).Desugar(),
			channel.NewMessageDispatcher(logging.FromContext(ctx).Desugar()),
			config.FanoutConfig,
			r.reporter,
		)
		if err != nil {
			logging.FromContext(ctx).Error("Failed to create a new fanout.MessageHandler", err)
			return err
		}
		r.multiChannelMessageHandler.SetChannelHandler(config.HostName, fanoutHandler)
	} else {
		// Just update the config if necessary.
		haveSubs := httpHandler.GetSubscriptions(ctx)

		// Ignore the closures, we stash the values that we can tell from if the values have actually changed.
		if diff := cmp.Diff(config.FanoutConfig.Subscriptions, haveSubs, cmpopts.IgnoreFields(kncloudevents.RetryConfig{}, "Backoff", "CheckRetry")); diff != "" {
			logging.FromContext(ctx).Info("Updating fanout config: ", zap.String("Diff", diff))
			httpHandler.SetSubscriptions(ctx, config.FanoutConfig.Subscriptions)
		}
	}

	// Look for an https handler that's configured to use paths
	httpsHandler := r.multiChannelMessageHandler.GetChannelHandler(config.Path)
	if httpsHandler == nil {
		// No handler yet, create one.
		fanoutHandler, err := fanout.NewFanoutMessageHandler(
			logging.FromContext(ctx).Desugar(),
			channel.NewMessageDispatcher(logging.FromContext(ctx).Desugar()),
			config.FanoutConfig,
			r.reporter,
			channel.ResolveMessageChannelFromPath(channel.ParseChannelFromPath),
		)
		if err != nil {
			logging.FromContext(ctx).Error("Failed to create a new fanout.MessageHandler", err)
			return err
		}
		r.multiChannelMessageHandler.SetChannelHandler(config.Path, fanoutHandler)
	} else {
		// Just update the config if necessary.
		haveSubs := httpsHandler.GetSubscriptions(ctx)

		// Ignore the closures, we stash the values that we can tell from if the values have actually changed.
		if diff := cmp.Diff(config.FanoutConfig.Subscriptions, haveSubs, cmpopts.IgnoreFields(kncloudevents.RetryConfig{}, "Backoff", "CheckRetry")); diff != "" {
			logging.FromContext(ctx).Info("Updating fanout config: ", zap.String("Diff", diff))
			httpsHandler.SetSubscriptions(ctx, config.FanoutConfig.Subscriptions)
		}
	}

	handleSubscribers(imc.Spec.Subscribers, kncloudevents.AddOrUpdateAddressableHandler)

	return nil
}

func (r *Reconciler) patchSubscriberStatus(ctx context.Context, imc *v1.InMemoryChannel) error {
	after := imc.DeepCopy()

	after.Status.Subscribers = make([]eventingduckv1.SubscriberStatus, 0)
	for _, sub := range imc.Spec.Subscribers {
		after.Status.Subscribers = append(after.Status.Subscribers, eventingduckv1.SubscriberStatus{
			UID:                sub.UID,
			ObservedGeneration: sub.Generation,
			Ready:              corev1.ConditionTrue,
		})
	}
	jsonPatch, err := duck.CreatePatch(imc, after)
	if err != nil {
		return fmt.Errorf("creating JSON patch: %w", err)
	}
	// If there is nothing to patch, we are good, just return.
	// Empty patch is [], hence we check for that.
	if len(jsonPatch) == 0 {
		return nil
	}

	patch, err := jsonPatch.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshaling JSON patch: %w", err)
	}
	patched, err := r.messagingClientSet.InMemoryChannels(imc.Namespace).Patch(ctx, imc.Name, types.JSONPatchType, patch, metav1.PatchOptions{}, "status")
	if err != nil {
		return fmt.Errorf("Failed patching: %w", err)
	}
	logging.FromContext(ctx).Debugw("Patched resource", zap.Any("patch", patch), zap.Any("patched", patched))
	return nil
}

// newConfigForInMemoryChannel creates a new Config for a single inmemory channel.
func newConfigForInMemoryChannel(imc *v1.InMemoryChannel) (*multichannelfanout.ChannelConfig, error) {
	subs := make([]fanout.Subscription, len(imc.Spec.Subscribers))

	for i, sub := range imc.Spec.Subscribers {
		conf, err := fanout.SubscriberSpecToFanoutConfig(sub)
		if err != nil {
			return nil, err
		}
		subs[i] = *conf
	}

	return &multichannelfanout.ChannelConfig{
		Namespace: imc.Namespace,
		Name:      imc.Name,
		HostName:  imc.Status.Address.URL.Host,
		Path:      fmt.Sprintf("%s/%s", imc.Namespace, imc.Name),
		FanoutConfig: fanout.Config{
			AsyncHandler:  true,
			Subscriptions: subs,
		},
	}, nil
}

func (r *Reconciler) deleteFunc(obj interface{}) {
	if obj == nil {
		return
	}
	acc, err := kmeta.DeletionHandlingAccessor(obj)
	if err != nil {
		return
	}
	imc, ok := acc.(*v1.InMemoryChannel)
	if !ok || imc == nil {
		return
	}
	if imc.Status.Address != nil && imc.Status.Address.URL != nil {
		if hostName := imc.Status.Address.URL.Host; hostName != "" {
			r.multiChannelMessageHandler.DeleteChannelHandler(hostName)
		}
	}

	handleSubscribers(imc.Spec.Subscribers, kncloudevents.DeleteAddressableHandler)
}

func handleSubscribers(subscribers []eventingduckv1.SubscriberSpec, handle func(duckv1.Addressable)) {
	for _, sub := range subscribers {
		handle(duckv1.Addressable{
			URL:     sub.SubscriberURI,
			CACerts: sub.SubscriberCACerts,
		})

		if sub.ReplyURI != nil {
			handle(duckv1.Addressable{
				URL:     sub.ReplyURI,
				CACerts: sub.ReplyCACerts,
			})
		}

		if sub.Delivery != nil && sub.Delivery.DeadLetterSink != nil && sub.Delivery.DeadLetterSink.URI != nil {
			handle(duckv1.Addressable{
				URL:     sub.Delivery.DeadLetterSink.URI,
				CACerts: sub.Delivery.DeadLetterSink.CACerts,
			})
		}
	}
}
