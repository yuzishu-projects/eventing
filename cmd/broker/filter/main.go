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

package main

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/kelseyhightower/envconfig"
	"go.uber.org/zap"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	configmap "knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"
	"knative.dev/pkg/signals"
	"knative.dev/pkg/system"
	"knative.dev/pkg/tracing"
	tracingconfig "knative.dev/pkg/tracing/config"

	"knative.dev/eventing/cmd/broker"
	"knative.dev/eventing/pkg/apis/feature"
	"knative.dev/eventing/pkg/broker/filter"
	triggerinformer "knative.dev/eventing/pkg/client/injection/informers/eventing/v1/trigger"
	"knative.dev/eventing/pkg/reconciler/names"
)

const (
	defaultMetricsPort = 9092
	component          = "mt_broker_filter"
)

type envConfig struct {
	Namespace string `envconfig:"NAMESPACE" required:"true"`
	// TODO: change this environment variable to something like "PodGroupName".
	PodName       string `envconfig:"POD_NAME" required:"true"`
	ContainerName string `envconfig:"CONTAINER_NAME" required:"true"`
	HTTPPort      int    `envconfig:"FILTER_PORT" default:"8080"`
	HTTPSPort     int    `envconfig:"FILTER_PORT_HTTPS" default:"8443"`
}

func main() {
	ctx := signals.NewContext()

	// Report stats on Go memory usage every 30 seconds.
	metrics.MemStatsOrDie(ctx)

	cfg := injection.ParseAndGetRESTConfigOrDie()

	var env envConfig
	if err := envconfig.Process("", &env); err != nil {
		log.Fatal("Failed to process env var", zap.Error(err))
	}

	log.Printf("Registering %d clients", len(injection.Default.GetClients()))
	log.Printf("Registering %d informer factories", len(injection.Default.GetInformerFactories()))
	log.Printf("Registering %d informers", len(injection.Default.GetInformers()))

	ctx, informers := injection.Default.SetupInformers(ctx, cfg)
	kubeClient := kubeclient.Get(ctx)

	loggingConfig, err := broker.GetLoggingConfig(ctx, system.Namespace(), logging.ConfigMapName())
	if err != nil {
		log.Fatal("Error loading/parsing logging configuration:", err)
	}
	sl, atomicLevel := logging.NewLoggerFromConfig(loggingConfig, component)
	logger := sl.Desugar()
	defer flush(sl)

	logger.Info("Starting the Broker Filter")

	// Watch the logging config map and dynamically update logging levels.
	configMapWatcher := configmap.NewInformedWatcher(kubeClient, system.Namespace())
	// Watch the observability config map and dynamically update metrics exporter.
	updateFunc, err := metrics.UpdateExporterFromConfigMapWithOpts(ctx, metrics.ExporterOptions{
		Component:      component,
		PrometheusPort: defaultMetricsPort,
	}, sl)
	if err != nil {
		logger.Fatal("Failed to create metrics exporter update function", zap.Error(err))
	}
	configMapWatcher.Watch(metrics.ConfigMapName(), updateFunc)
	// TODO change the component name to broker once Stackdriver metrics are approved.
	// Watch the observability config map and dynamically update request logs.
	configMapWatcher.Watch(logging.ConfigMapName(), logging.UpdateLevelFromConfigMap(sl, atomicLevel, component))

	featureStore := feature.NewStore(logging.FromContext(ctx).Named("feature-config-store"))
	featureStore.WatchConfigs(configMapWatcher)

	// Decorate contexts with the current state of the feature config.
	ctxFunc := func(ctx context.Context) context.Context {
		return featureStore.ToContext(ctx)
	}

	bin := fmt.Sprintf("%s.%s", names.BrokerFilterName, system.Namespace())
	tracer, err := tracing.SetupPublishingWithDynamicConfig(sl, configMapWatcher, bin, tracingconfig.ConfigName)
	if err != nil {
		logger.Fatal("Error setting up trace publishing", zap.Error(err))
	}

	reporter := filter.NewStatsReporter(env.ContainerName, kmeta.ChildName(env.PodName, uuid.New().String()))

	// We are running both the receiver (takes messages in from the Broker) and the dispatcher (send
	// the messages to the triggers' subscribers) in this binary.
	handler, err := filter.NewHandler(logger, triggerinformer.Get(ctx), reporter, ctxFunc)
	if err != nil {
		logger.Fatal("Error creating Handler", zap.Error(err))
	}
	serverManager, err := filter.NewServerManager(ctx, logger, configMapWatcher, env.HTTPPort, env.HTTPSPort, handler)
	if err != nil {
		logger.Fatal("Error creating server manager", zap.Error(err))
	}

	// configMapWatcher does not block, so start it first.
	if err = configMapWatcher.Start(ctx.Done()); err != nil {
		logger.Warn("Failed to start ConfigMap watcher", zap.Error(err))
	}

	// Start all of the informers and wait for them to sync.
	logger.Info("Starting informers.")
	if err := controller.StartInformers(ctx.Done(), informers...); err != nil {
		logger.Fatal("Failed to start informers", zap.Error(err))
	}

	// Start the servers
	logger.Info("Filter starting...")
	err = serverManager.StartServers(ctx)
	if err != nil {
		logger.Fatal("serverManager.StartServers() returned an error", zap.Error(err))
	}
	tracer.Shutdown(context.Background())
	logger.Info("Exiting...")
}

func flush(logger *zap.SugaredLogger) {
	_ = logger.Sync()
	metrics.FlushExporter()
}
