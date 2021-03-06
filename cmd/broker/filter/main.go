/*
 * Copyright 2019 The Knative Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"flag"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/kelseyhightower/envconfig"
	"go.opencensus.io/stats/view"
	"go.uber.org/zap"

	"knative.dev/eventing/cmd/broker"
	"knative.dev/eventing/pkg/broker/filter"
	cmpresources "knative.dev/eventing/pkg/reconciler/configmappropagation/resources"
	namespaceresources "knative.dev/eventing/pkg/reconciler/namespace/resources"
	"knative.dev/eventing/pkg/tracing"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"
	"knative.dev/pkg/signals"
	tracingconfig "knative.dev/pkg/tracing/config"

	"knative.dev/pkg/injection/sharedmain"

	eventingv1alpha1 "knative.dev/eventing/pkg/client/clientset/versioned"
	eventinginformers "knative.dev/eventing/pkg/client/informers/externalversions"
)

var (
	masterURL  = flag.String("master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	kubeconfig = flag.String("kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
)

const (
	defaultMetricsPort = 9092
	component          = "broker_filter"
)

type envConfig struct {
	Broker    string `envconfig:"BROKER" required:"true"`
	Namespace string `envconfig:"NAMESPACE" required:"true"`
	// TODO: change this environment variable to something like "PodGroupName".
	PodName       string `envconfig:"POD_NAME" required:"true"`
	ContainerName string `envconfig:"CONTAINER_NAME" required:"true"`
}

func main() {
	flag.Parse()

	ctx := signals.NewContext()

	// Report stats on Go memory usage every 30 seconds.
	msp := metrics.NewMemStatsAll()
	msp.Start(ctx, 30*time.Second)
	if err := view.Register(msp.DefaultViews()...); err != nil {
		log.Fatalf("Error exporting go memstats view: %v", err)
	}

	cfg, err := sharedmain.GetConfig(*masterURL, *kubeconfig)
	if err != nil {
		log.Fatal("Error building kubeconfig", err)
	}

	var env envConfig
	if err := envconfig.Process("", &env); err != nil {
		log.Fatal("Failed to process env var", zap.Error(err))
	}

	ctx, _ = injection.Default.SetupInformers(ctx, cfg)
	kubeClient := kubeclient.Get(ctx)

	loggingConfigMapName := cmpresources.MakeCopyConfigMapName(namespaceresources.DefaultConfigMapPropagationName, logging.ConfigMapName())
	metricsConfigMapName := cmpresources.MakeCopyConfigMapName(namespaceresources.DefaultConfigMapPropagationName, metrics.ConfigMapName())

	loggingConfig, err := broker.GetLoggingConfig(ctx, env.Namespace, loggingConfigMapName)
	if err != nil {
		log.Fatal("Error loading/parsing logging configuration:", err)
	}
	sl, atomicLevel := logging.NewLoggerFromConfig(loggingConfig, component)
	logger := sl.Desugar()
	defer flush(sl)

	logger.Info("Starting the Broker Filter")

	eventingClient := eventingv1alpha1.NewForConfigOrDie(cfg)
	eventingFactory := eventinginformers.NewSharedInformerFactoryWithOptions(eventingClient,
		controller.GetResyncPeriod(ctx),
		eventinginformers.WithNamespace(env.Namespace))
	triggerInformer := eventingFactory.Eventing().V1alpha1().Triggers()

	// Watch the logging config map and dynamically update logging levels.
	configMapWatcher := configmap.NewInformedWatcher(kubeClient, env.Namespace)
	// Watch the observability config map and dynamically update metrics exporter.
	updateFunc, err := metrics.UpdateExporterFromConfigMapWithOpts(metrics.ExporterOptions{
		Component:      component,
		PrometheusPort: defaultMetricsPort,
	}, sl)
	if err != nil {
		logger.Fatal("Failed to create metrics exporter update function", zap.Error(err))
	}
	configMapWatcher.Watch(metricsConfigMapName, updateFunc)
	// TODO change the component name to broker once Stackdriver metrics are approved.
	// Watch the observability config map and dynamically update request logs.
	configMapWatcher.Watch(loggingConfigMapName, logging.UpdateLevelFromConfigMap(sl, atomicLevel, component))

	bin := tracing.BrokerFilterName(tracing.BrokerFilterNameArgs{
		Namespace:  env.Namespace,
		BrokerName: env.Broker,
	})
	if err = tracing.SetupDynamicPublishing(sl, configMapWatcher, bin,
		cmpresources.MakeCopyConfigMapName(namespaceresources.DefaultConfigMapPropagationName, tracingconfig.ConfigName)); err != nil {
		logger.Fatal("Error setting up trace publishing", zap.Error(err))
	}

	reporter := filter.NewStatsReporter(env.ContainerName, kmeta.ChildName(env.PodName, uuid.New().String()))

	// We are running both the receiver (takes messages in from the Broker) and the dispatcher (send
	// the messages to the triggers' subscribers) in this binary.
	handler, err := filter.NewHandler(logger, triggerInformer.Lister().Triggers(env.Namespace), reporter)
	if err != nil {
		logger.Fatal("Error creating Handler", zap.Error(err))
	}

	// configMapWatcher does not block, so start it first.
	if err = configMapWatcher.Start(ctx.Done()); err != nil {
		logger.Warn("Failed to start ConfigMap watcher", zap.Error(err))
	}

	// Start all of the informers and wait for them to sync.
	logger.Info("Starting informer.")

	go eventingFactory.Start(ctx.Done())
	eventingFactory.WaitForCacheSync(ctx.Done())

	// Start blocks forever.
	logger.Info("Filter starting...")

	err = handler.Start(ctx)
	if err != nil {
		logger.Fatal("handler.Start() returned an error", zap.Error(err))
	}
	logger.Info("Exiting...")
}

func flush(logger *zap.SugaredLogger) {
	_ = logger.Sync()
	metrics.FlushExporter()
}
