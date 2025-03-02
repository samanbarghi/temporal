// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package temporal

import (
	"context"
	"fmt"
	"strings"

	"github.com/pborman/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	otelresource "go.opentelemetry.io/otel/sdk/resource"
	otelsdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.10.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"google.golang.org/grpc"

	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/client"
	"go.temporal.io/server/common/authorization"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/collection"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/headers"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/cassandra"
	persistenceClient "go.temporal.io/server/common/persistence/client"
	"go.temporal.io/server/common/persistence/sql"
	esclient "go.temporal.io/server/common/persistence/visibility/store/elasticsearch/client"
	"go.temporal.io/server/common/pprof"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/common/resolver"
	"go.temporal.io/server/common/resource"
	"go.temporal.io/server/common/rpc/encryption"
	"go.temporal.io/server/common/searchattribute"
	"go.temporal.io/server/common/telemetry"
	"go.temporal.io/server/service/frontend"
	"go.temporal.io/server/service/history"
	"go.temporal.io/server/service/history/replication"
	"go.temporal.io/server/service/history/workflow"
	"go.temporal.io/server/service/matching"
	"go.temporal.io/server/service/worker"
)

type (
	ServicesGroupOut struct {
		fx.Out
		Services *ServicesMetadata `group:"services"`
	}

	ServicesGroupIn struct {
		fx.In
		Services []*ServicesMetadata `group:"services"`
	}

	ServicesMetadata struct {
		app         *fx.App
		serviceName primitives.ServiceName
		logger      log.Logger
		stopChan    chan struct{}
	}

	ServerFx struct {
		app                        *fx.App
		startupSynchronizationMode synchronizationModeParams
		logger                     log.Logger
	}

	serverOptionsProvider struct {
		fx.Out
		ServerOptions              *serverOptions
		StopChan                   chan interface{}
		StartupSynchronizationMode synchronizationModeParams

		Config      *config.Config
		PProfConfig *config.PProf
		LogConfig   log.Config

		ServiceNames    resource.ServiceNames
		NamespaceLogger resource.NamespaceLogger

		ServiceResolver        resolver.ServiceResolver
		CustomDataStoreFactory persistenceClient.AbstractDataStoreFactory

		SearchAttributesMapper searchattribute.Mapper
		CustomInterceptors     []grpc.UnaryServerInterceptor
		Authorizer             authorization.Authorizer
		ClaimMapper            authorization.ClaimMapper
		AudienceGetter         authorization.JWTAudienceMapper

		// below are things that could be over write by server options or may have default if not supplied by serverOptions.
		Logger                log.Logger
		ClientFactoryProvider client.FactoryProvider
		DynamicConfigClient   dynamicconfig.Client
		TLSConfigProvider     encryption.TLSConfigProvider
		EsConfig              *esclient.Config
		EsClient              esclient.Client
		MetricsHandler        metrics.Handler
	}
)

func NewServerFx(opts ...ServerOption) (*ServerFx, error) {
	var s ServerFx
	s.app = fx.New(
		pprof.Module,
		fx.Provide(NewServerFxImpl),
		fx.Supply(opts),
		fx.Provide(ServerOptionsProvider),
		TraceExportModule,

		fx.Provide(PersistenceFactoryProvider),
		fx.Provide(HistoryServiceProvider),
		fx.Provide(MatchingServiceProvider),
		fx.Provide(FrontendServiceProvider),
		fx.Provide(InternalFrontendServiceProvider),
		fx.Provide(WorkerServiceProvider),

		fx.Provide(ApplyClusterMetadataConfigProvider),
		fx.Invoke(ServerLifetimeHooks),
		FxLogAdapter,

		fx.Populate(&s.startupSynchronizationMode),
		fx.Populate(&s.logger),
	)
	if err := s.app.Err(); err != nil {
		return nil, err
	}
	return &s, nil
}

func ServerOptionsProvider(opts []ServerOption) (serverOptionsProvider, error) {
	so := newServerOptions(opts)

	err := so.loadAndValidate()
	if err != nil {
		return serverOptionsProvider{}, err
	}

	persistenceConfig := so.config.Persistence
	err = verifyPersistenceCompatibleVersion(persistenceConfig, so.persistenceServiceResolver)
	if err != nil {
		return serverOptionsProvider{}, err
	}

	stopChan := make(chan interface{})

	// Logger
	logger := so.logger
	if logger == nil {
		logger = log.NewZapLogger(log.BuildZapLogger(so.config.Log))
	}

	// ClientFactoryProvider
	clientFactoryProvider := so.clientFactoryProvider
	if clientFactoryProvider == nil {
		clientFactoryProvider = client.NewFactoryProvider()
	}

	// MetricsHandler
	metricHandler := so.metricHandler
	if metricHandler == nil {
		metricHandler = metrics.MetricsHandlerFromConfig(logger, so.config.Global.Metrics)
	}

	// DynamicConfigClient
	dcClient := so.dynamicConfigClient
	if dcClient == nil {
		dcConfig := so.config.DynamicConfigClient
		if dcConfig != nil {
			dcClient, err = dynamicconfig.NewFileBasedClient(dcConfig, logger, stopChan)
			if err != nil {
				return serverOptionsProvider{}, fmt.Errorf("unable to create dynamic config client: %w", err)
			}
		} else {
			// noop client
			logger.Info("Dynamic config client is not configured. Using default values.")
			dcClient = dynamicconfig.NewNoopClient()
		}
	}

	// TLSConfigProvider
	tlsConfigProvider := so.tlsConfigProvider
	if tlsConfigProvider == nil {
		tlsConfigProvider, err = encryption.NewTLSConfigProviderFromConfig(so.config.Global.TLS, metricHandler, logger, nil)
		if err != nil {
			return serverOptionsProvider{}, err
		}
	}

	// EsConfig / EsClient
	var esConfig *esclient.Config
	var esClient esclient.Client

	if persistenceConfig.StandardVisibilityConfigExist() &&
		persistenceConfig.DataStores[persistenceConfig.VisibilityStore].Elasticsearch != nil {
		esConfig = persistenceConfig.DataStores[persistenceConfig.VisibilityStore].Elasticsearch
	} else if persistenceConfig.SecondaryVisibilityConfigExist() &&
		persistenceConfig.DataStores[persistenceConfig.SecondaryVisibilityStore].Elasticsearch != nil {
		esConfig = persistenceConfig.DataStores[persistenceConfig.SecondaryVisibilityStore].Elasticsearch
	} else if persistenceConfig.AdvancedVisibilityConfigExist() {
		esConfig = persistenceConfig.DataStores[persistenceConfig.AdvancedVisibilityStore].Elasticsearch
	}

	if esConfig != nil {
		esHttpClient := so.elasticsearchHttpClient
		if esHttpClient == nil {
			var err error
			esHttpClient, err = esclient.NewAwsHttpClient(esConfig.AWSRequestSigning)
			if err != nil {
				return serverOptionsProvider{}, fmt.Errorf("unable to create AWS HTTP client for Elasticsearch: %w", err)
			}
		}

		esClient, err = esclient.NewClient(esConfig, esHttpClient, logger)
		if err != nil {
			return serverOptionsProvider{}, fmt.Errorf("unable to create Elasticsearch client: %w", err)
		}
	}

	return serverOptionsProvider{
		ServerOptions:              so,
		StopChan:                   stopChan,
		StartupSynchronizationMode: so.startupSynchronizationMode,

		Config:      so.config,
		PProfConfig: &so.config.Global.PProf,
		LogConfig:   so.config.Log,

		ServiceNames:    so.serviceNames,
		NamespaceLogger: so.namespaceLogger,

		ServiceResolver:        so.persistenceServiceResolver,
		CustomDataStoreFactory: so.customDataStoreFactory,

		SearchAttributesMapper: so.searchAttributesMapper,
		CustomInterceptors:     so.customInterceptors,
		Authorizer:             so.authorizer,
		ClaimMapper:            so.claimMapper,
		AudienceGetter:         so.audienceGetter,

		Logger:                logger,
		ClientFactoryProvider: clientFactoryProvider,
		DynamicConfigClient:   dcClient,
		TLSConfigProvider:     tlsConfigProvider,
		EsConfig:              esConfig,
		EsClient:              esClient,
		MetricsHandler:        metricHandler,
	}, nil
}

// Start temporal server.
// This function should be called only once, Server doesn't support multiple restarts.
func (s *ServerFx) Start() error {
	err := s.app.Start(context.Background())
	if err != nil {
		return err
	}

	if s.startupSynchronizationMode.blockingStart {
		// If s.so.interruptCh is nil this will wait forever.
		interruptSignal := <-s.startupSynchronizationMode.interruptCh
		s.logger.Info("Received interrupt signal, stopping the server.", tag.Value(interruptSignal))
		return s.Stop()
	}

	return nil
}

// Stop stops the server.
func (s *ServerFx) Stop() error {
	return s.app.Stop(context.Background())
}

func (svc *ServicesMetadata) Stop(ctx context.Context) {
	stopCtx, cancelFunc := context.WithTimeout(ctx, serviceStopTimeout)
	defer cancelFunc()
	err := svc.app.Stop(stopCtx)
	if err != nil {
		svc.logger.Error("Failed to stop service", tag.Service(svc.serviceName), tag.Error(err))
	}

	// verify "Start" goroutine returned
	select {
	case <-svc.stopChan:
	case <-stopCtx.Done():
		svc.logger.Error("Timed out waiting for service to stop", tag.Service(svc.serviceName), tag.NewDurationTag("timeout", serviceStopTimeout))
	}
}

type (
	ServiceProviderParamsCommon struct {
		fx.In

		Cfg                        *config.Config
		ServiceNames               resource.ServiceNames
		Logger                     log.Logger
		NamespaceLogger            resource.NamespaceLogger
		DynamicConfigClient        dynamicconfig.Client
		MetricsHandler             metrics.Handler
		EsConfig                   *esclient.Config
		EsClient                   esclient.Client
		TlsConfigProvider          encryption.TLSConfigProvider
		PersistenceConfig          config.Persistence
		ClusterMetadata            *cluster.Config
		ClientFactoryProvider      client.FactoryProvider
		AudienceGetter             authorization.JWTAudienceMapper
		PersistenceServiceResolver resolver.ServiceResolver
		PersistenceFactoryProvider persistenceClient.FactoryProviderFn
		SearchAttributesMapper     searchattribute.Mapper
		CustomInterceptors         []grpc.UnaryServerInterceptor
		Authorizer                 authorization.Authorizer
		ClaimMapper                authorization.ClaimMapper
		DataStoreFactory           persistenceClient.AbstractDataStoreFactory
		SpanExporters              []otelsdktrace.SpanExporter
		InstanceID                 resource.InstanceID `optional:"true"`
	}
)

func HistoryServiceProvider(
	params ServiceProviderParamsCommon,
) (ServicesGroupOut, error) {
	serviceName := primitives.HistoryService

	if _, ok := params.ServiceNames[serviceName]; !ok {
		params.Logger.Info("Service is not requested, skipping initialization.", tag.Service(serviceName))
		return ServicesGroupOut{}, nil
	}

	stopChan := make(chan struct{})
	app := fx.New(
		fx.Supply(
			stopChan,
			params.EsConfig,
			params.PersistenceConfig,
			params.ClusterMetadata,
			params.Cfg,
			serviceName,
		),
		fx.Provide(func() persistenceClient.AbstractDataStoreFactory { return params.DataStoreFactory }),
		fx.Provide(func() client.FactoryProvider { return params.ClientFactoryProvider }),
		fx.Provide(func() authorization.JWTAudienceMapper { return params.AudienceGetter }),
		fx.Provide(func() resolver.ServiceResolver { return params.PersistenceServiceResolver }),
		fx.Provide(func() searchattribute.Mapper { return params.SearchAttributesMapper }),
		fx.Provide(func() []grpc.UnaryServerInterceptor { return params.CustomInterceptors }),
		fx.Provide(func() authorization.Authorizer { return params.Authorizer }),
		fx.Provide(func() authorization.ClaimMapper { return params.ClaimMapper }),
		fx.Provide(func() encryption.TLSConfigProvider { return params.TlsConfigProvider }),
		fx.Provide(func() dynamicconfig.Client { return params.DynamicConfigClient }),
		fx.Provide(func() log.Logger { return params.Logger }),
		fx.Provide(resource.DefaultSnTaggedLoggerProvider),
		fx.Provide(func() metrics.Handler {
			return params.MetricsHandler.WithTags(metrics.ServiceNameTag(serviceName))
		}),
		fx.Provide(func() esclient.Client { return params.EsClient }),
		fx.Provide(params.PersistenceFactoryProvider),
		fx.Provide(workflow.NewTaskGeneratorProvider),
		fx.Supply(params.SpanExporters),
		ServiceTracingModule,
		resource.DefaultOptions,
		history.QueueModule,
		history.Module,
		replication.Module,
		FxLogAdapter,
	)

	return ServicesGroupOut{
		Services: &ServicesMetadata{
			app:         app,
			serviceName: serviceName,
			logger:      params.Logger,
			stopChan:    stopChan,
		},
	}, app.Err()
}

func MatchingServiceProvider(
	params ServiceProviderParamsCommon,
) (ServicesGroupOut, error) {
	serviceName := primitives.MatchingService

	if _, ok := params.ServiceNames[serviceName]; !ok {
		params.Logger.Info("Service is not requested, skipping initialization.", tag.Service(serviceName))
		return ServicesGroupOut{}, nil
	}

	stopChan := make(chan struct{})
	app := fx.New(
		fx.Supply(
			stopChan,
			params.EsConfig,
			params.PersistenceConfig,
			params.ClusterMetadata,
			params.Cfg,
			serviceName,
		),
		fx.Provide(func() persistenceClient.AbstractDataStoreFactory { return params.DataStoreFactory }),
		fx.Provide(func() client.FactoryProvider { return params.ClientFactoryProvider }),
		fx.Provide(func() authorization.JWTAudienceMapper { return params.AudienceGetter }),
		fx.Provide(func() resolver.ServiceResolver { return params.PersistenceServiceResolver }),
		fx.Provide(func() searchattribute.Mapper { return params.SearchAttributesMapper }),
		fx.Provide(func() []grpc.UnaryServerInterceptor { return params.CustomInterceptors }),
		fx.Provide(func() authorization.Authorizer { return params.Authorizer }),
		fx.Provide(func() authorization.ClaimMapper { return params.ClaimMapper }),
		fx.Provide(func() encryption.TLSConfigProvider { return params.TlsConfigProvider }),
		fx.Provide(func() dynamicconfig.Client { return params.DynamicConfigClient }),
		fx.Provide(func() log.Logger { return params.Logger }),
		fx.Provide(resource.DefaultSnTaggedLoggerProvider),
		fx.Provide(func() metrics.Handler {
			return params.MetricsHandler.WithTags(metrics.ServiceNameTag(serviceName))
		}),
		fx.Provide(func() esclient.Client { return params.EsClient }),
		fx.Provide(params.PersistenceFactoryProvider),
		fx.Supply(params.SpanExporters),
		ServiceTracingModule,
		resource.DefaultOptions,
		matching.Module,
		FxLogAdapter,
	)

	return ServicesGroupOut{
		Services: &ServicesMetadata{
			app:         app,
			serviceName: serviceName,
			logger:      params.Logger,
			stopChan:    stopChan,
		},
	}, app.Err()
}

func FrontendServiceProvider(
	params ServiceProviderParamsCommon,
) (ServicesGroupOut, error) {
	return genericFrontendServiceProvider(params, primitives.FrontendService)
}

func InternalFrontendServiceProvider(
	params ServiceProviderParamsCommon,
) (ServicesGroupOut, error) {
	return genericFrontendServiceProvider(params, primitives.InternalFrontendService)
}

func genericFrontendServiceProvider(
	params ServiceProviderParamsCommon,
	serviceName primitives.ServiceName,
) (ServicesGroupOut, error) {
	if _, ok := params.ServiceNames[serviceName]; !ok {
		params.Logger.Info("Service is not requested, skipping initialization.", tag.Service(serviceName))
		return ServicesGroupOut{}, nil
	}

	stopChan := make(chan struct{})
	app := fx.New(
		fx.Supply(
			stopChan,
			params.EsConfig,
			params.PersistenceConfig,
			params.ClusterMetadata,
			params.Cfg,
			serviceName,
		),
		fx.Provide(func() persistenceClient.AbstractDataStoreFactory { return params.DataStoreFactory }),
		fx.Provide(func() client.FactoryProvider { return params.ClientFactoryProvider }),
		fx.Provide(func() authorization.JWTAudienceMapper { return params.AudienceGetter }),
		fx.Provide(func() resolver.ServiceResolver { return params.PersistenceServiceResolver }),
		fx.Provide(func() searchattribute.Mapper { return params.SearchAttributesMapper }),
		fx.Provide(func() []grpc.UnaryServerInterceptor { return params.CustomInterceptors }),
		fx.Provide(func() authorization.Authorizer { return params.Authorizer }),
		fx.Provide(func() authorization.ClaimMapper {
			switch serviceName {
			case primitives.FrontendService:
				return params.ClaimMapper
			case primitives.InternalFrontendService:
				return authorization.NewNoopClaimMapper()
			default:
				panic("Unexpected frontend service name")
			}
		}),
		fx.Provide(func() encryption.TLSConfigProvider { return params.TlsConfigProvider }),
		fx.Provide(func() dynamicconfig.Client { return params.DynamicConfigClient }),
		fx.Provide(func() log.Logger { return params.Logger }),
		fx.Provide(func() log.SnTaggedLogger {
			// Use "frontend" for logs even if serviceName is "internal-frontend", but add an
			// extra tag to differentiate.
			tags := []tag.Tag{tag.Service(primitives.FrontendService)}
			if serviceName == primitives.InternalFrontendService {
				tags = append(tags, tag.NewBoolTag("internal-frontend", true))
			}
			return log.With(params.Logger, tags...)
		}),
		fx.Provide(func() metrics.Handler {
			// Use either "frontend" or "internal-frontend" for metrics
			return params.MetricsHandler.WithTags(metrics.ServiceNameTag(serviceName))
		}),
		fx.Provide(func() resource.NamespaceLogger { return params.NamespaceLogger }),
		fx.Provide(func() esclient.Client { return params.EsClient }),
		fx.Provide(params.PersistenceFactoryProvider),
		fx.Supply(params.SpanExporters),
		ServiceTracingModule,
		resource.DefaultOptions,
		frontend.Module,
		FxLogAdapter,
	)

	return ServicesGroupOut{
		Services: &ServicesMetadata{
			app:         app,
			serviceName: serviceName,
			logger:      params.Logger,
			stopChan:    stopChan,
		},
	}, app.Err()
}

func WorkerServiceProvider(
	params ServiceProviderParamsCommon,
) (ServicesGroupOut, error) {
	serviceName := primitives.WorkerService

	if _, ok := params.ServiceNames[serviceName]; !ok {
		params.Logger.Info("Service is not requested, skipping initialization.", tag.Service(serviceName))
		return ServicesGroupOut{}, nil
	}

	stopChan := make(chan struct{})
	app := fx.New(
		fx.Supply(
			stopChan,
			params.EsConfig,
			params.PersistenceConfig,
			params.ClusterMetadata,
			params.Cfg,
			serviceName,
		),
		fx.Provide(func() persistenceClient.AbstractDataStoreFactory { return params.DataStoreFactory }),
		fx.Provide(func() client.FactoryProvider { return params.ClientFactoryProvider }),
		fx.Provide(func() authorization.JWTAudienceMapper { return params.AudienceGetter }),
		fx.Provide(func() resolver.ServiceResolver { return params.PersistenceServiceResolver }),
		fx.Provide(func() searchattribute.Mapper { return params.SearchAttributesMapper }),
		fx.Provide(func() []grpc.UnaryServerInterceptor { return params.CustomInterceptors }),
		fx.Provide(func() authorization.Authorizer { return params.Authorizer }),
		fx.Provide(func() authorization.ClaimMapper { return params.ClaimMapper }),
		fx.Provide(func() encryption.TLSConfigProvider { return params.TlsConfigProvider }),
		fx.Provide(func() dynamicconfig.Client { return params.DynamicConfigClient }),
		fx.Provide(func() log.Logger { return params.Logger }),
		fx.Provide(resource.DefaultSnTaggedLoggerProvider),
		fx.Provide(func() metrics.Handler {
			return params.MetricsHandler.WithTags(metrics.ServiceNameTag(serviceName))
		}),
		fx.Provide(func() esclient.Client { return params.EsClient }),
		fx.Provide(params.PersistenceFactoryProvider),
		fx.Supply(params.SpanExporters),
		ServiceTracingModule,
		resource.DefaultOptions,
		worker.Module,
		FxLogAdapter,
	)

	return ServicesGroupOut{
		Services: &ServicesMetadata{
			app:         app,
			serviceName: serviceName,
			logger:      params.Logger,
			stopChan:    stopChan,
		},
	}, app.Err()
}

// ApplyClusterMetadataConfigProvider performs a config check against the configured persistence store for cluster metadata.
// If there is a mismatch, the persisted values take precedence and will be written over in the config objects.
// This is to keep this check hidden from downstream calls.
// TODO: move this to cluster.fx
func ApplyClusterMetadataConfigProvider(
	logger log.Logger,
	config *config.Config,
	persistenceServiceResolver resolver.ServiceResolver,
	persistenceFactoryProvider persistenceClient.FactoryProviderFn,
	customDataStoreFactory persistenceClient.AbstractDataStoreFactory,
) (*cluster.Config, config.Persistence, error) {
	ctx := context.TODO()
	logger = log.With(logger, tag.ComponentMetadataInitializer)

	clusterName := persistenceClient.ClusterName(config.ClusterMetadata.CurrentClusterName)
	dataStoreFactory, _ := persistenceClient.DataStoreFactoryProvider(
		clusterName,
		persistenceServiceResolver,
		&config.Persistence,
		customDataStoreFactory,
		logger,
		nil,
	)
	factory := persistenceFactoryProvider(persistenceClient.NewFactoryParams{
		DataStoreFactory:           dataStoreFactory,
		Cfg:                        &config.Persistence,
		PersistenceMaxQPS:          nil,
		PersistenceNamespaceMaxQPS: nil,
		EnablePriorityRateLimiting: nil,
		ClusterName:                persistenceClient.ClusterName(config.ClusterMetadata.CurrentClusterName),
		MetricsHandler:             nil,
		Logger:                     logger,
	})
	defer factory.Close()

	clusterMetadataManager, err := factory.NewClusterMetadataManager()
	if err != nil {
		return config.ClusterMetadata, config.Persistence, fmt.Errorf("error initializing cluster metadata manager: %w", err)
	}
	defer clusterMetadataManager.Close()

	var sqlIndexNames []string
	initialIndexSearchAttributes := make(map[string]*persistencespb.IndexSearchAttributes)
	if ds := config.Persistence.GetVisibilityStoreConfig(); ds.SQL != nil {
		indexName := ds.GetIndexName()
		sqlIndexNames = append(sqlIndexNames, indexName)
		initialIndexSearchAttributes[indexName] = searchattribute.GetSqlDbIndexSearchAttributes()
	}
	if ds := config.Persistence.GetSecondaryVisibilityStoreConfig(); ds.SQL != nil {
		indexName := ds.GetIndexName()
		sqlIndexNames = append(sqlIndexNames, indexName)
		initialIndexSearchAttributes[indexName] = searchattribute.GetSqlDbIndexSearchAttributes()
	}

	clusterData := config.ClusterMetadata
	for clusterName, clusterInfo := range clusterData.ClusterInformation {
		if clusterName != clusterData.CurrentClusterName {
			logger.Warn(
				"ClusterInformation in ClusterMetadata config is deprecated. "+
					"Please use TCTL admin tool to configure remote cluster connections",
				tag.Key("clusterInformation"),
				tag.ClusterName(clusterName),
				tag.IgnoredValue(clusterInfo))

			// Only configure current cluster metadata from static config file
			continue
		}

		var clusterId string
		if uuid.Parse(clusterInfo.ClusterID) == nil {
			if clusterInfo.ClusterID != "" {
				logger.Warn("Cluster Id in Cluster Metadata config is not a valid uuid. Generating a new Cluster Id")
			}
			clusterId = uuid.New()
		} else {
			clusterId = clusterInfo.ClusterID
		}

		applied, err := clusterMetadataManager.SaveClusterMetadata(
			ctx,
			&persistence.SaveClusterMetadataRequest{
				ClusterMetadata: persistencespb.ClusterMetadata{
					HistoryShardCount:        config.Persistence.NumHistoryShards,
					ClusterName:              clusterName,
					ClusterId:                clusterId,
					ClusterAddress:           clusterInfo.RPCAddress,
					FailoverVersionIncrement: clusterData.FailoverVersionIncrement,
					InitialFailoverVersion:   clusterInfo.InitialFailoverVersion,
					IsGlobalNamespaceEnabled: clusterData.EnableGlobalNamespace,
					IsConnectionEnabled:      clusterInfo.Enabled,
					UseClusterIdMembership:   true, // Enable this for new cluster after 1.19. This is to prevent two clusters join into one ring.
					IndexSearchAttributes:    initialIndexSearchAttributes,
				},
			})
		if err != nil {
			logger.Warn("Failed to save cluster metadata.", tag.Error(err), tag.ClusterName(clusterName))
		}
		if applied {
			logger.Info("Successfully saved cluster metadata.", tag.ClusterName(clusterName))
			continue
		}

		resp, err := clusterMetadataManager.GetClusterMetadata(
			ctx,
			&persistence.GetClusterMetadataRequest{ClusterName: clusterName},
		)
		if err != nil {
			return config.ClusterMetadata, config.Persistence, fmt.Errorf("error while fetching cluster metadata: %w", err)
		}
		currentMetadata := resp.ClusterMetadata

		// TODO (rodrigozhou): Remove this block for v1.21.
		// Handle registering custom search attributes when upgrading to v1.20.
		if len(sqlIndexNames) > 0 {
			needSave := false
			if currentMetadata.IndexSearchAttributes == nil {
				currentMetadata.IndexSearchAttributes = initialIndexSearchAttributes
				needSave = true
			} else {
				for _, indexName := range sqlIndexNames {
					if _, ok := currentMetadata.IndexSearchAttributes[indexName]; !ok {
						currentMetadata.IndexSearchAttributes[indexName] = searchattribute.GetSqlDbIndexSearchAttributes()
						needSave = true
					}
				}
			}

			if needSave {
				_, err := clusterMetadataManager.SaveClusterMetadata(
					ctx,
					&persistence.SaveClusterMetadataRequest{
						ClusterMetadata: currentMetadata,
						Version:         resp.Version,
					},
				)
				if err != nil {
					logger.Warn(
						"Failed to register search attributes.",
						tag.Error(err),
						tag.ClusterName(clusterName),
					)
				}
				logger.Info("Successfully registered search attributes.", tag.ClusterName(clusterName))

				// need to re-fetch cluster metadata since it might need to be updated again below
				resp, err = clusterMetadataManager.GetClusterMetadata(
					ctx,
					&persistence.GetClusterMetadataRequest{ClusterName: clusterName},
				)
				if err != nil {
					return config.ClusterMetadata, config.Persistence, fmt.Errorf("error while fetching cluster metadata: %w", err)
				}
				currentMetadata = resp.ClusterMetadata
			}
		}

		// Allow updating cluster metadata if global namespace is disabled
		if !resp.IsGlobalNamespaceEnabled && clusterData.EnableGlobalNamespace {
			currentMetadata.IsGlobalNamespaceEnabled = clusterData.EnableGlobalNamespace
			currentMetadata.InitialFailoverVersion = clusterInfo.InitialFailoverVersion
			currentMetadata.FailoverVersionIncrement = clusterData.FailoverVersionIncrement

			applied, err := clusterMetadataManager.SaveClusterMetadata(
				ctx,
				&persistence.SaveClusterMetadataRequest{
					ClusterMetadata: currentMetadata,
					Version:         resp.Version,
				})
			if !applied || err != nil {
				return config.ClusterMetadata, config.Persistence, fmt.Errorf("error while updating cluster metadata: %w", err)
			}
		} else if resp.IsGlobalNamespaceEnabled != clusterData.EnableGlobalNamespace {
			logger.Warn(
				mismatchLogMessage,
				tag.Key("clusterMetadata.EnableGlobalNamespace"),
				tag.IgnoredValue(clusterData.EnableGlobalNamespace),
				tag.Value(resp.IsGlobalNamespaceEnabled))
			config.ClusterMetadata.EnableGlobalNamespace = resp.IsGlobalNamespaceEnabled
		}

		// Verify current cluster metadata
		persistedShardCount := resp.HistoryShardCount
		if config.Persistence.NumHistoryShards != persistedShardCount {
			logger.Warn(
				mismatchLogMessage,
				tag.Key("persistence.numHistoryShards"),
				tag.IgnoredValue(config.Persistence.NumHistoryShards),
				tag.Value(persistedShardCount))
			config.Persistence.NumHistoryShards = persistedShardCount
		}
		if resp.FailoverVersionIncrement != clusterData.FailoverVersionIncrement {
			logger.Warn(
				mismatchLogMessage,
				tag.Key("clusterMetadata.FailoverVersionIncrement"),
				tag.IgnoredValue(clusterData.FailoverVersionIncrement),
				tag.Value(resp.FailoverVersionIncrement))
			config.ClusterMetadata.FailoverVersionIncrement = resp.FailoverVersionIncrement
		}
	}
	err = loadClusterInformationFromStore(ctx, config, clusterMetadataManager, logger)
	if err != nil {
		return config.ClusterMetadata, config.Persistence, fmt.Errorf("error while loading metadata from cluster: %w", err)
	}
	return config.ClusterMetadata, config.Persistence, nil
}

func PersistenceFactoryProvider() persistenceClient.FactoryProviderFn {
	return persistenceClient.FactoryProvider
}

// TODO: move this to cluster.fx
func loadClusterInformationFromStore(ctx context.Context, config *config.Config, clusterMsg persistence.ClusterMetadataManager, logger log.Logger) error {
	iter := collection.NewPagingIterator(func(paginationToken []byte) ([]interface{}, []byte, error) {
		request := &persistence.ListClusterMetadataRequest{
			PageSize:      100,
			NextPageToken: nil,
		}
		resp, err := clusterMsg.ListClusterMetadata(ctx, request)
		if err != nil {
			return nil, nil, err
		}
		var pageItem []interface{}
		for _, metadata := range resp.ClusterMetadata {
			pageItem = append(pageItem, metadata)
		}
		return pageItem, resp.NextPageToken, nil
	})

	for iter.HasNext() {
		item, err := iter.Next()
		if err != nil {
			return err
		}
		metadata := item.(*persistence.GetClusterMetadataResponse)
		shardCount := metadata.HistoryShardCount
		if shardCount == 0 {
			// This is to add backward compatibility to the config based cluster connection.
			shardCount = config.Persistence.NumHistoryShards
		}
		newMetadata := cluster.ClusterInformation{
			Enabled:                metadata.IsConnectionEnabled,
			InitialFailoverVersion: metadata.InitialFailoverVersion,
			RPCAddress:             metadata.ClusterAddress,
			ShardCount:             shardCount,
		}
		if staticClusterMetadata, ok := config.ClusterMetadata.ClusterInformation[metadata.ClusterName]; ok {
			if metadata.ClusterName != config.ClusterMetadata.CurrentClusterName {
				logger.Warn(
					"ClusterInformation in ClusterMetadata config is deprecated. Please use TCTL tool to configure remote cluster connections",
					tag.Key("clusterInformation"),
					tag.IgnoredValue(staticClusterMetadata),
					tag.Value(newMetadata))
			} else {
				newMetadata.RPCAddress = staticClusterMetadata.RPCAddress
				logger.Info(fmt.Sprintf("Use rpc address %v for cluster %v.", newMetadata.RPCAddress, metadata.ClusterName))
			}
		}
		config.ClusterMetadata.ClusterInformation[metadata.ClusterName] = newMetadata
	}
	return nil
}

func ServerLifetimeHooks(
	lc fx.Lifecycle,
	svr *ServerImpl,
) {
	lc.Append(fx.StartStopHook(svr.Start, svr.Stop))
}

func verifyPersistenceCompatibleVersion(config config.Persistence, persistenceServiceResolver resolver.ServiceResolver) error {
	// cassandra schema version validation
	if err := cassandra.VerifyCompatibleVersion(config, persistenceServiceResolver); err != nil {
		return fmt.Errorf("cassandra schema version compatibility check failed: %w", err)
	}
	// sql schema version validation
	if err := sql.VerifyCompatibleVersion(config, persistenceServiceResolver); err != nil {
		return fmt.Errorf("sql schema version compatibility check failed: %w", err)
	}
	return nil
}

// TraceExportModule holds process-global telemetry fx state defining the set of
// OTEL trace/span exporters used by tracing instrumentation. The following
// types can be overriden/augmented with fx.Replace/fx.Decorate:
//
// - []go.opentelemetry.io/otel/sdk/trace.SpanExporter
var TraceExportModule = fx.Options(
	fx.Invoke(func(log log.Logger) {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(
			func(err error) {
				log.Warn("OTEL error", tag.Error(err), tag.ErrorType(err))
			}),
		)
	}),

	fx.Provide(func(lc fx.Lifecycle, c *config.Config) ([]otelsdktrace.SpanExporter, error) {
		exporters, err := c.ExporterConfig.SpanExporters()
		if err != nil {
			return nil, err
		}
		lc.Append(fx.Hook{
			OnStart: startAll(exporters),
			OnStop:  shutdownAll(exporters),
		})
		return exporters, nil
	}),
)

// ServiceTracingModule holds per-service (i.e. frontend/history/matching/worker) fx
// state. The following types can be overriden with fx.Replace/fx.Decorate:
//
//   - []go.opentelemetry.io/otel/sdk/trace.BatchSpanProcessorOption
//     default: empty slice
//   - []go.opentelemetry.io/otel/sdk/trace.SpanProcessor
//     default: wrap each otelsdktrace.SpanExporter with otelsdktrace.NewBatchSpanProcessor
//   - *go.opentelemetry.io/otel/sdk/resource.Resource
//     default: resource.Default() augmented with the supplied serviceName
//   - []go.opentelemetry.io/otel/sdk/trace.TracerProviderOption
//     default: the provided resource.Resource and each of the otelsdktrace.SpanExporter
//   - go.opentelemetry.io/otel/trace.TracerProvider
//     default: otelsdktrace.NewTracerProvider with each of the otelsdktrace.TracerProviderOption
//   - go.opentelemetry.io/otel/ppropagation.TextMapPropagator
//     default: propagation.TraceContext{}
//   - telemetry.ServerTraceInterceptor
//   - telemetry.ClientTraceInterceptor
var ServiceTracingModule = fx.Options(
	fx.Supply([]otelsdktrace.BatchSpanProcessorOption{}),
	fx.Provide(
		fx.Annotate(
			func(exps []otelsdktrace.SpanExporter, opts []otelsdktrace.BatchSpanProcessorOption) []otelsdktrace.SpanProcessor {
				sps := make([]otelsdktrace.SpanProcessor, 0, len(exps))
				for _, exp := range exps {
					sps = append(sps, otelsdktrace.NewBatchSpanProcessor(exp, opts...))
				}
				return sps
			},
			fx.ParamTags(`optional:"true"`, ``),
		),
	),
	fx.Provide(
		fx.Annotate(
			func(rsn primitives.ServiceName, rsi resource.InstanceID) (*otelresource.Resource, error) {
				// map "internal-frontend" to "frontend" for the purpose of tracing
				if rsn == primitives.InternalFrontendService {
					rsn = primitives.FrontendService
				}
				serviceName := string(rsn)
				if !strings.HasPrefix(serviceName, "io.temporal.") {
					serviceName = fmt.Sprintf("io.temporal.%s", serviceName)
				}
				attrs := []attribute.KeyValue{
					semconv.ServiceNameKey.String(serviceName),
					semconv.ServiceVersionKey.String(headers.ServerVersion),
				}
				if rsi != "" {
					attrs = append(attrs, semconv.ServiceInstanceIDKey.String(string(rsi)))
				}
				return otelresource.New(context.Background(),
					otelresource.WithProcess(),
					otelresource.WithOS(),
					otelresource.WithHost(),
					otelresource.WithContainer(),
					otelresource.WithAttributes(attrs...),
				)
			},
			fx.ParamTags(``, `optional:"true"`),
		),
	),
	fx.Provide(
		func(r *otelresource.Resource, sps []otelsdktrace.SpanProcessor) []otelsdktrace.TracerProviderOption {
			opts := make([]otelsdktrace.TracerProviderOption, 0, len(sps)+1)
			opts = append(opts, otelsdktrace.WithResource(r))
			for _, sp := range sps {
				opts = append(opts, otelsdktrace.WithSpanProcessor(sp))
			}
			return opts
		},
	),
	fx.Provide(func(lc fx.Lifecycle, opts []otelsdktrace.TracerProviderOption) trace.TracerProvider {
		tp := otelsdktrace.NewTracerProvider(opts...)
		lc.Append(fx.Hook{OnStop: func(ctx context.Context) error {
			return tp.Shutdown(ctx)
		}})
		return tp
	}),
	// Haven't had use for baggage propagation yet
	fx.Provide(func() propagation.TextMapPropagator { return propagation.TraceContext{} }),
	fx.Provide(telemetry.NewServerTraceInterceptor),
	fx.Provide(telemetry.NewClientTraceInterceptor),
)

func startAll(exporters []otelsdktrace.SpanExporter) func(ctx context.Context) error {
	type starter interface{ Start(context.Context) error }
	return func(ctx context.Context) error {
		for _, e := range exporters {
			if starter, ok := e.(starter); ok {
				err := starter.Start(ctx)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
}

func shutdownAll(exporters []otelsdktrace.SpanExporter) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		for _, e := range exporters {
			err := e.Shutdown(ctx)
			if err != nil {
				return err
			}
		}
		return nil
	}
}

var FxLogAdapter = fx.WithLogger(func(logger log.Logger) fxevent.Logger {
	return &fxLogAdapter{logger: logger}
})

type fxLogAdapter struct {
	logger log.Logger
}

func (l *fxLogAdapter) LogEvent(e fxevent.Event) {
	switch e := e.(type) {
	case *fxevent.OnStartExecuting:
		l.logger.Debug("OnStart hook executing",
			tag.ComponentFX,
			tag.NewStringTag("callee", e.FunctionName),
			tag.NewStringTag("caller", e.CallerName),
		)
	case *fxevent.OnStartExecuted:
		if e.Err != nil {
			l.logger.Error("OnStart hook failed",
				tag.ComponentFX,
				tag.NewStringTag("callee", e.FunctionName),
				tag.NewStringTag("caller", e.CallerName),
				tag.Error(e.Err),
			)
		} else {
			l.logger.Debug("OnStart hook executed",
				tag.ComponentFX,
				tag.NewStringTag("callee", e.FunctionName),
				tag.NewStringTag("caller", e.CallerName),
				tag.NewStringTag("runtime", e.Runtime.String()),
			)
		}
	case *fxevent.OnStopExecuting:
		l.logger.Debug("OnStop hook executing",
			tag.ComponentFX,
			tag.NewStringTag("callee", e.FunctionName),
			tag.NewStringTag("caller", e.CallerName),
		)
	case *fxevent.OnStopExecuted:
		if e.Err != nil {
			l.logger.Error("OnStop hook failed",
				tag.ComponentFX,
				tag.NewStringTag("callee", e.FunctionName),
				tag.NewStringTag("caller", e.CallerName),
				tag.Error(e.Err),
			)
		} else {
			l.logger.Debug("OnStop hook executed",
				tag.ComponentFX,
				tag.NewStringTag("callee", e.FunctionName),
				tag.NewStringTag("caller", e.CallerName),
				tag.NewStringTag("runtime", e.Runtime.String()),
			)
		}
	case *fxevent.Supplied:
		if e.Err != nil {
			l.logger.Error("supplied",
				tag.ComponentFX,
				tag.NewStringTag("type", e.TypeName),
				tag.NewStringTag("module", e.ModuleName),
				tag.Error(e.Err))
		}
	case *fxevent.Provided:
		if e.Err != nil {
			l.logger.Error("error encountered while applying options",
				tag.ComponentFX,
				tag.NewStringTag("module", e.ModuleName),
				tag.Error(e.Err))
		}
	case *fxevent.Replaced:
		if e.Err != nil {
			l.logger.Error("error encountered while replacing",
				tag.ComponentFX,
				tag.NewStringTag("module", e.ModuleName),
				tag.Error(e.Err))
		}
	case *fxevent.Decorated:
		if e.Err != nil {
			l.logger.Error("error encountered while applying options",
				tag.ComponentFX,
				tag.NewStringTag("module", e.ModuleName),
				tag.Error(e.Err))
		}
	case *fxevent.Invoking:
		// Do not log stack as it will make logs hard to read.
		l.logger.Debug("invoking",
			tag.ComponentFX,
			tag.NewStringTag("function", e.FunctionName),
			tag.NewStringTag("module", e.ModuleName),
		)
	case *fxevent.Invoked:
		if e.Err != nil {
			l.logger.Error("invoke failed",
				tag.ComponentFX,
				tag.Error(e.Err),
				tag.NewStringTag("stack", e.Trace),
				tag.NewStringTag("function", e.FunctionName),
				tag.NewStringTag("module", e.ModuleName),
			)
		}
	case *fxevent.Stopping:
		l.logger.Info("received signal",
			tag.ComponentFX,
			tag.NewStringTag("signal", e.Signal.String()))
	case *fxevent.Stopped:
		if e.Err != nil {
			l.logger.Error("stop failed", tag.ComponentFX, tag.Error(e.Err))
		}
	case *fxevent.RollingBack:
		l.logger.Error("start failed, rolling back", tag.ComponentFX, tag.Error(e.StartErr))
	case *fxevent.RolledBack:
		if e.Err != nil {
			l.logger.Error("rollback failed", tag.ComponentFX, tag.Error(e.Err))
		}
	case *fxevent.Started:
		if e.Err != nil {
			l.logger.Error("start failed", tag.ComponentFX, tag.Error(e.Err))
		} else {
			l.logger.Debug("started", tag.ComponentFX)
		}
	case *fxevent.LoggerInitialized:
		if e.Err != nil {
			l.logger.Error("custom logger initialization failed", tag.ComponentFX, tag.Error(e.Err))
		} else {
			l.logger.Debug("initialized custom fxevent.Logger",
				tag.ComponentFX,
				tag.NewStringTag("function", e.ConstructorName))
		}
	default:
		l.logger.Warn("unknown fx log type, update fxLogAdapter",
			tag.ComponentFX,
			tag.ValueType(e),
		)
	}
}
