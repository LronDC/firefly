/*
Copyright 2022 The Firefly Authors.

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

package app

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"

	karmadaversioned "github.com/karmada-io/karmada/pkg/generated/clientset/versioned"
	karmadainformers "github.com/karmada-io/karmada/pkg/generated/informers/externalversions"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	discovery "k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/configz"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	genericcontrollermanager "k8s.io/controller-manager/app"
	"k8s.io/controller-manager/controller"
	controllerhealthz "k8s.io/controller-manager/pkg/healthz"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/controller-manager/pkg/leadermigration"
	"k8s.io/klog/v2"

	"github.com/carlory/firefly/cmd/firefly-karmada-manager/app/config"
	"github.com/carlory/firefly/cmd/firefly-karmada-manager/app/options"
	"github.com/carlory/firefly/pkg/clientbuilder"
	fireflyversioned "github.com/carlory/firefly/pkg/generated/clientset/versioned"
	fireflyinformers "github.com/carlory/firefly/pkg/generated/informers/externalversions"
	fireflyctrlmgrconfig "github.com/carlory/firefly/pkg/karmada/controller/apis/config"
	karmadafireflyinformers "github.com/carlory/firefly/pkg/karmada/generated/informers/externalversions"
)

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

const (
	// ControllerStartJitter is the Jitter used when starting controller managers
	ControllerStartJitter = 1.0
	// ConfigzName is the name used for register firefly-controller manager /configz, same with GroupName.
	ConfigzName = "fireflycontrollermanager.config.firefly.io"
)

// NewControllerManagerCommand creates a *cobra.Command object with default parameters
func NewControllerManagerCommand() *cobra.Command {
	s, err := options.NewFireflyControllerManagerOptions()
	if err != nil {
		klog.Fatalf("unable to initialize command options: %v", err)
	}
	cmd := &cobra.Command{
		Use: "firefly-karmada-manager",
		Long: `The Firefly controller manager is a daemon that embeds
the core control loops shipped with Kubernetes. In applications of robotics and
automation, a control loop is a non-terminating loop that regulates the state of
the system. In Kubernetes, a controller is a control loop that watches the shared
state of the cluster through the apiserver and makes changes attempting to move the
current state towards the desired state. Examples of controllers that ship with
Kubernetes today are the replication controller, endpoints controller, namespace
controller, and serviceaccounts controller.`,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			// silence client-go warnings.
			// kube-controller-manager generically watches APIs (including deprecated ones),
			// and CI ensures it works properly against matching kube-apiserver versions.
			restclient.SetDefaultWarningHandler(restclient.NoWarnings{})
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()

			// Activate logging as soon as possible, after that
			// show flags with the final logging configuration.
			if err := logsapi.ValidateAndApply(s.Logs, utilfeature.DefaultFeatureGate); err != nil {
				return err
			}
			cliflag.PrintFlags(cmd.Flags())

			c, err := s.Config(KnownControllers(), ControllersDisabledByDefault.List())
			if err != nil {
				return err
			}

			return Run(c.Complete(), wait.NeverStop)
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags(KnownControllers(), ControllersDisabledByDefault.List())
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, namedFlagSets, cols)

	return cmd
}

// ResyncPeriod returns a function which generates a duration each time it is
// invoked; this is so that multiple controllers don't get into lock-step and all
// hammer the apiserver with list requests simultaneously.
func ResyncPeriod(c *config.CompletedConfig) func() time.Duration {
	return func() time.Duration {
		factor := rand.Float64() + 1
		return time.Duration(float64(c.ComponentConfig.Generic.MinResyncPeriod.Nanoseconds()) * factor)
	}
}

// Run runs the FireflyControllerManagerOptions.
func Run(c *config.CompletedConfig, stopCh <-chan struct{}) error {
	// To help debugging, immediately log version
	klog.Infof("Version: %+v", version.Get())

	klog.InfoS("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	// Start events processing pipeline.
	c.EventBroadcaster.StartStructuredLogging(0)
	c.EventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: c.KarmadaKubeClient.CoreV1().Events("")})
	defer c.EventBroadcaster.Shutdown()

	if cfgz, err := configz.New(ConfigzName); err == nil {
		cfgz.Set(c.ComponentConfig)
	} else {
		klog.Errorf("unable to register configz: %v", err)
	}

	// Setup any healthz checks we will want to use.
	var checks []healthz.HealthChecker
	var electionChecker *leaderelection.HealthzAdaptor
	if c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		electionChecker = leaderelection.NewLeaderHealthzAdaptor(time.Second * 20)
		checks = append(checks, electionChecker)
	}
	healthzHandler := controllerhealthz.NewMutableHealthzHandler(checks...)

	// Start the controller manager HTTP server
	// unsecuredMux is the handler for these controller *after* authn/authz filters have been applied
	var unsecuredMux *mux.PathRecorderMux
	if c.SecureServing != nil {
		unsecuredMux = genericcontrollermanager.NewBaseHandler(&c.ComponentConfig.Generic.Debugging, healthzHandler)
		handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, &c.Authorization, &c.Authentication)
		// TODO: handle stoppedCh and listenerStoppedCh returned by c.SecureServing.Serve
		if _, _, err := c.SecureServing.Serve(handler, 0, stopCh); err != nil {
			return err
		}
	}

	karmadaClientBuilder, fireflyKubeClientBuilder := createClientBuilders(c)

	run := func(ctx context.Context, initializersFunc ControllerInitializersFunc) {
		controllerContext, err := CreateControllerContext(c, karmadaClientBuilder, fireflyKubeClientBuilder, ctx.Done())
		if err != nil {
			klog.Fatalf("error building controller context: %v", err)
		}
		controllerInitializers := initializersFunc()
		if err := StartControllers(ctx, controllerContext, controllerInitializers, unsecuredMux, healthzHandler); err != nil {
			klog.Fatalf("error starting controllers: %v", err)
		}

		controllerContext.KarmadaDynamicInformerFactory.Start(stopCh)
		controllerContext.KarmadaKubeInformerFactory.Start(stopCh)
		controllerContext.KarmadaInformerFactory.Start(stopCh)
		controllerContext.KarmadaFireflyInformerFactory.Start(stopCh)
		controllerContext.FireflyDynamicInformerFactory.Start(stopCh)
		controllerContext.FireflyKubeInformerFactory.Start(stopCh)
		controllerContext.FireflyInformerFactory.Start(stopCh)
		controllerContext.ObjectOrMetadataInformerFactory.Start(stopCh)
		close(controllerContext.InformersStarted)

		<-ctx.Done()
	}

	// No leader election, run directly
	if !c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		ctx, _ := wait.ContextForChannel(stopCh)
		run(ctx, NewControllerInitializers)
		return nil
	}

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// add a uniquifier so that two processes on the same host don't accidentally both become active
	id = id + "_" + string(uuid.NewUUID())

	// leaderMigrator will be non-nil if and only if Leader Migration is enabled.
	var leaderMigrator *leadermigration.LeaderMigrator = nil

	// If leader migration is enabled, create the LeaderMigrator and prepare for migration
	if leadermigration.Enabled(&c.ComponentConfig.Generic) {
		klog.Infof("starting leader migration")

		leaderMigrator = leadermigration.NewLeaderMigrator(&c.ComponentConfig.Generic.LeaderMigration, "firefly-controller-manager")
	}

	// Start the main lock
	go leaderElectAndRun(c, id, electionChecker,
		c.ComponentConfig.Generic.LeaderElection.ResourceLock,
		c.ComponentConfig.Generic.LeaderElection.ResourceName,
		leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				initializersFunc := NewControllerInitializers
				if leaderMigrator != nil {
					// If leader migration is enabled, we should start only non-migrated controllers
					//  for the main lock.
					initializersFunc = createInitializersFunc(leaderMigrator.FilterFunc, leadermigration.ControllerNonMigrated)
					klog.Info("leader migration: starting main controllers.")
				}
				run(ctx, initializersFunc)
			},
			OnStoppedLeading: func() {
				klog.ErrorS(nil, "leaderelection lost")
				klog.FlushAndExit(klog.ExitFlushTimeout, 1)
			},
		})

	// If Leader Migration is enabled, proceed to attempt the migration lock.
	if leaderMigrator != nil {
		// Wait for Service Account Token Controller to start before acquiring the migration lock.
		// At this point, the main lock must have already been acquired, or the KCM process already exited.
		// We wait for the main lock before acquiring the migration lock to prevent the situation
		//  where KCM instance A holds the main lock while KCM instance B holds the migration lock.
		<-leaderMigrator.MigrationReady

		// Start the migration lock.
		go leaderElectAndRun(c, id, electionChecker,
			c.ComponentConfig.Generic.LeaderMigration.ResourceLock,
			c.ComponentConfig.Generic.LeaderMigration.LeaderName,
			leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					klog.Info("leader migration: starting migrated controllers.")
					// DO NOT start saTokenController under migration lock
					run(ctx, createInitializersFunc(leaderMigrator.FilterFunc, leadermigration.ControllerMigrated))
				},
				OnStoppedLeading: func() {
					klog.ErrorS(nil, "migration leaderelection lost")
					klog.FlushAndExit(klog.ExitFlushTimeout, 1)
				},
			})
	}

	<-stopCh
	return nil
}

// ControllerContext defines the context object for controller
type ControllerContext struct {
	// KarmadaClientBuilder will provide a client for this controller to use
	KarmadaClientBuilder clientbuilder.KarmadaControllerClientBuilder

	// FireflyClientBuilder will provide a client for this controller to use
	FireflyClientBuilder clientbuilder.FireflyControllerClientBuilder

	// KarmadaKubeInformerFactory gives access to dynamic informers for the controller.
	KarmadaDynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory

	// KarmadaKubeInformerFactory gives access to kubernetes informers for the controller.
	KarmadaKubeInformerFactory informers.SharedInformerFactory

	// KarmadaFireflyInformerFactory gives access to karmada's firelfy informers for the controller.
	KarmadaFireflyInformerFactory karmadafireflyinformers.SharedInformerFactory

	// KarmadaInformerFactory gives access to firefly informers for the controller.
	KarmadaInformerFactory karmadainformers.SharedInformerFactory

	// FireflyDynamicInformerFactory gives access to dynamic informers for the controller.
	FireflyDynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory

	// FireflyKubeInformerFactory gives access to firefly informers for the controller.
	FireflyKubeInformerFactory informers.SharedInformerFactory

	// FireflyInformerFactory gives access to firefly informers for the controller.
	FireflyInformerFactory fireflyinformers.SharedInformerFactory

	// EstimatorNamespace is the namespace of scheduler-estimator
	EstimatorNamespace string
	// KarmadaName is the name of a firefly karmada object
	KarmadaName string

	// ObjectOrMetadataInformerFactory gives access to informers for typed resources
	// and dynamic resources by their metadata. All generic controllers currently use
	// object metadata - if a future controller needs access to the full object this
	// would become GenericInformerFactory and take a dynamic client.
	ObjectOrMetadataInformerFactory informerfactory.InformerFactory

	// ComponentConfig provides access to init options for a given controller
	ComponentConfig fireflyctrlmgrconfig.FireflyKarmadaManagerConfiguration

	// DeferredDiscoveryRESTMapper is a RESTMapper that will defer
	// initialization of the RESTMapper until the first mapping is
	// requested.
	RESTMapper *restmapper.DeferredDiscoveryRESTMapper

	// AvailableResources is a map listing currently available resources
	AvailableResources map[schema.GroupVersionResource]bool

	// HostClusterAvailableResources is a map listing currently available resources on the host cluster.
	HostClusterAvailableResources map[schema.GroupVersionResource]bool

	// InformersStarted is closed after all of the controllers have been initialized and are running.  After this point it is safe,
	// for an individual controller to start the shared informers. Before it is closed, they should not.
	InformersStarted chan struct{}

	// ResyncPeriod generates a duration each time it is invoked; this is so that
	// multiple controllers don't get into lock-step and all hammer the apiserver
	// with list requests simultaneously.
	ResyncPeriod func() time.Duration
}

// IsControllerEnabled checks if the context's controllers enabled or not
func (c ControllerContext) IsControllerEnabled(name string) bool {
	return genericcontrollermanager.IsControllerEnabled(name, ControllersDisabledByDefault, c.ComponentConfig.Generic.Controllers)
}

// InitFunc is used to launch a particular controller. It returns a controller
// that can optionally implement other interfaces so that the controller manager
// can support the requested features.
// The returned controller may be nil, which will be considered an anonymous controller
// that requests no additional features from the controller manager.
// Any error returned will cause the controller process to `Fatal`
// The bool indicates whether the controller was enabled.
type InitFunc func(ctx context.Context, controllerCtx ControllerContext) (controller controller.Interface, enabled bool, err error)

// ControllerInitializersFunc is used to create a collection of initializers.
type ControllerInitializersFunc func() (initializers map[string]InitFunc)

// KnownControllers returns all known controllers's name
func KnownControllers() []string {
	ret := sets.StringKeySet(NewControllerInitializers())
	return ret.List()
}

// ControllersDisabledByDefault is the set of controllers which is disabled by default
var ControllersDisabledByDefault = sets.NewString()

// NewControllerInitializers is a public map of named controller groups (you can start more than one in an init func)
// paired to their InitFunc.  This allows for structured downstream composition and subdivision.
func NewControllerInitializers() map[string]InitFunc {
	controllers := map[string]InitFunc{}
	controllers["estimator"] = startEstimatorController
	controllers["node"] = startNodeController
	controllers["foo"] = startFooController
	controllers["kubean"] = startKubeanController
	return controllers
}

// GetKarmadaAvailableResources gets the map which contains all available resources of the karmada-apiserver
// TODO: In general, any controller checking this needs to be dynamic so
// users don't have to restart their controller manager if they change the apiserver.
// Until we get there, the structure here needs to be exposed for the construction of a proper ControllerContext.
func GetKarmadaAvailableResources(clientBuilder clientbuilder.KarmadaControllerClientBuilder) (map[schema.GroupVersionResource]bool, error) {
	client := clientBuilder.ClientOrDie("controller-discovery")
	discoveryClient := client.Discovery()
	return GetAvailableResources(discoveryClient)
}

// GetHostClusterAvailableResources gets the map which contains all available resources of the kube-apiserver
// on the host cluster.
// TODO: In general, any controller checking this needs to be dynamic so
// users don't have to restart their controller manager if they change the apiserver.
// Until we get there, the structure here needs to be exposed for the construction of a proper ControllerContext.
func GetHostClusterAvailableResources(clientBuilder clientbuilder.FireflyControllerClientBuilder) (map[schema.GroupVersionResource]bool, error) {
	client := clientBuilder.ClientOrDie("controller-discovery")
	discoveryClient := client.Discovery()
	return GetAvailableResources(discoveryClient)
}

// GetAvailableResources gets the map which contains all available resources of the apiserver
// TODO: In general, any controller checking this needs to be dynamic so
// users don't have to restart their controller manager if they change the apiserver.
// Until we get there, the structure here needs to be exposed for the construction of a proper ControllerContext.
func GetAvailableResources(client discovery.DiscoveryInterface) (map[schema.GroupVersionResource]bool, error) {
	_, resourceMap, err := client.ServerGroupsAndResources()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to get all supported resources from server: %v", err))
	}
	if len(resourceMap) == 0 {
		return nil, fmt.Errorf("unable to get any supported resources from server")
	}

	allResources := map[schema.GroupVersionResource]bool{}
	for _, apiResourceList := range resourceMap {
		version, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			return nil, err
		}
		for _, apiResource := range apiResourceList.APIResources {
			allResources[version.WithResource(apiResource.Name)] = true
		}
	}

	return allResources, nil
}

// CreateControllerContext creates a context struct containing references to resources needed by the
// controllers such as the cloud provider and clientBuilder. rootClientBuilder is only used for
// the shared-informers client and token controller.
func CreateControllerContext(s *config.CompletedConfig, karmadaClientBuilder clientbuilder.KarmadaControllerClientBuilder, fireflyKubeClientBuilder clientbuilder.FireflyControllerClientBuilder, stop <-chan struct{}) (ControllerContext, error) {
	karmadaKubeClient := karmadaClientBuilder.ClientOrDie("karmada-kube-shared-informers")
	karmadaKubeSharedInformers := informers.NewSharedInformerFactory(karmadaKubeClient, ResyncPeriod(s)())

	karmadaDynamicClient := karmadaClientBuilder.DynamicClientOrDie("karmada-dynamic-shared-informers")
	karmadaDynamicSharedInformers := dynamicinformer.NewDynamicSharedInformerFactory(karmadaDynamicClient, ResyncPeriod(s)())

	karmadaFireflyClient := karmadaClientBuilder.KarmadaFireflyClientOrDie("karmada-firefly-shared-informers")
	karmadaFireflySharedInformers := karmadafireflyinformers.NewSharedInformerFactory(karmadaFireflyClient, ResyncPeriod(s)())

	clientConfig := karmadaClientBuilder.ConfigOrDie("karmada-shared-informers")
	karmadaClient := karmadaversioned.NewForConfigOrDie(clientConfig)
	karmadaSharedInformers := karmadainformers.NewSharedInformerFactory(karmadaClient, ResyncPeriod(s)())

	metadataClient := metadata.NewForConfigOrDie(karmadaClientBuilder.ConfigOrDie("firefly-metadata-informers"))
	metadataInformers := metadatainformer.NewSharedInformerFactory(metadataClient, ResyncPeriod(s)())

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	if err := genericcontrollermanager.WaitForAPIServer(karmadaKubeClient, 10*time.Second); err != nil {
		return ControllerContext{}, fmt.Errorf("failed to wait for apiserver being healthy: %v", err)
	}

	fireflyKubeClient := fireflyKubeClientBuilder.ClientOrDie("firefly-kube-shared-informers")
	fireflyKubeSharedInformers := informers.NewSharedInformerFactory(fireflyKubeClient, ResyncPeriod(s)())

	fireflyDynamicClient := fireflyKubeClientBuilder.DynamicClientOrDie("firefly-dynamic-shared-informers")
	fireflyDynamicSharedInformers := dynamicinformer.NewDynamicSharedInformerFactory(fireflyDynamicClient, ResyncPeriod(s)())

	fireflyClientConfig := fireflyKubeClientBuilder.ConfigOrDie("firefly-shared-informers")
	fireflyClient := fireflyversioned.NewForConfigOrDie(fireflyClientConfig)
	fireflySharedInformers := fireflyinformers.NewFilteredSharedInformerFactory(fireflyClient, ResyncPeriod(s)(), s.EstimatorNamespace, nil)

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	if err := genericcontrollermanager.WaitForAPIServer(fireflyKubeClient, 10*time.Second); err != nil {
		return ControllerContext{}, fmt.Errorf("failed to wait for apiserver being healthy: %v", err)
	}

	// Use a discovery client capable of being refreshed.
	discoveryClient := karmadaClientBuilder.DiscoveryClientOrDie("firelfy-controller-discovery")
	cachedClient := cacheddiscovery.NewMemCacheClient(discoveryClient)
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)
	go wait.Until(func() {
		restMapper.Reset()
	}, 30*time.Second, stop)

	availableResources, err := GetKarmadaAvailableResources(karmadaClientBuilder)
	if err != nil {
		return ControllerContext{}, err
	}

	hostClusterAvailableResources, err := GetHostClusterAvailableResources(fireflyKubeClientBuilder)
	if err != nil {
		return ControllerContext{}, err
	}

	ctx := ControllerContext{
		KarmadaClientBuilder:            karmadaClientBuilder,
		FireflyClientBuilder:            fireflyKubeClientBuilder,
		KarmadaDynamicInformerFactory:   karmadaDynamicSharedInformers,
		KarmadaKubeInformerFactory:      karmadaKubeSharedInformers,
		KarmadaFireflyInformerFactory:   karmadaFireflySharedInformers,
		KarmadaInformerFactory:          karmadaSharedInformers,
		FireflyDynamicInformerFactory:   fireflyDynamicSharedInformers,
		FireflyKubeInformerFactory:      fireflyKubeSharedInformers,
		FireflyInformerFactory:          fireflySharedInformers,
		ObjectOrMetadataInformerFactory: informerfactory.NewInformerFactory(karmadaKubeSharedInformers, metadataInformers),
		ComponentConfig:                 s.ComponentConfig,
		EstimatorNamespace:              s.EstimatorNamespace,
		KarmadaName:                     s.KarmadaName,
		RESTMapper:                      restMapper,
		AvailableResources:              availableResources,
		HostClusterAvailableResources:   hostClusterAvailableResources,
		InformersStarted:                make(chan struct{}),
		ResyncPeriod:                    ResyncPeriod(s),
	}
	return ctx, nil
}

// StartControllers starts a set of controllers with a specified ControllerContext
func StartControllers(ctx context.Context, controllerCtx ControllerContext, controllers map[string]InitFunc,
	unsecuredMux *mux.PathRecorderMux, healthzHandler *controllerhealthz.MutableHealthzHandler) error {
	var controllerChecks []healthz.HealthChecker

	for controllerName, initFn := range controllers {
		if !controllerCtx.IsControllerEnabled(controllerName) {
			klog.Warningf("%q is disabled", controllerName)
			continue
		}

		time.Sleep(wait.Jitter(controllerCtx.ComponentConfig.Generic.ControllerStartInterval.Duration, ControllerStartJitter))

		klog.V(1).Infof("Starting %q", controllerName)
		ctrl, started, err := initFn(ctx, controllerCtx)
		if err != nil {
			klog.Errorf("Error starting %q", controllerName)
			return err
		}
		if !started {
			klog.Warningf("Skipping %q", controllerName)
			continue
		}
		check := controllerhealthz.NamedPingChecker(controllerName)
		if ctrl != nil {
			// check if the controller supports and requests a debugHandler
			// and it needs the unsecuredMux to mount the handler onto.
			if debuggable, ok := ctrl.(controller.Debuggable); ok && unsecuredMux != nil {
				if debugHandler := debuggable.DebuggingHandler(); debugHandler != nil {
					basePath := "/debug/controllers/" + controllerName
					unsecuredMux.UnlistedHandle(basePath, http.StripPrefix(basePath, debugHandler))
					unsecuredMux.UnlistedHandlePrefix(basePath+"/", http.StripPrefix(basePath, debugHandler))
				}
			}
			if healthCheckable, ok := ctrl.(controller.HealthCheckable); ok {
				if realCheck := healthCheckable.HealthChecker(); realCheck != nil {
					check = controllerhealthz.NamedHealthChecker(controllerName, realCheck)
				}
			}
		}
		controllerChecks = append(controllerChecks, check)

		klog.Infof("Started %q", controllerName)
	}

	healthzHandler.AddHealthChecker(controllerChecks...)
	return nil
}

// createClientBuilders creates karmadaClientBuilder and fireflyKubeClientBuilder from the given configuration
func createClientBuilders(c *config.CompletedConfig) (karmadaClientBuilder clientbuilder.KarmadaControllerClientBuilder, fireflyKubeClientBuilder clientbuilder.FireflyControllerClientBuilder) {
	karmadaClientBuilder = clientbuilder.NewSimpleKarmadaControllerClientBuilder(c.KarmadaKubeconfig)

	fireflyKubeClientBuilder = clientbuilder.NewSimpleFireflyControllerClientBuilder(c.FireflyKubeconfig)
	return
}

// leaderElectAndRun runs the leader election, and runs the callbacks once the leader lease is acquired.
// TODO: extract this function into staging/controller-manager
func leaderElectAndRun(c *config.CompletedConfig, lockIdentity string, electionChecker *leaderelection.HealthzAdaptor, resourceLock string, leaseName string, callbacks leaderelection.LeaderCallbacks) {
	rl, err := resourcelock.NewFromKubeconfig(resourceLock,
		c.ComponentConfig.Generic.LeaderElection.ResourceNamespace,
		leaseName,
		resourcelock.ResourceLockConfig{
			Identity:      lockIdentity,
			EventRecorder: c.EventRecorder,
		},
		c.KarmadaKubeconfig,
		c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration)
	if err != nil {
		klog.Fatalf("error creating lock: %v", err)
	}

	leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: c.ComponentConfig.Generic.LeaderElection.LeaseDuration.Duration,
		RenewDeadline: c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration,
		RetryPeriod:   c.ComponentConfig.Generic.LeaderElection.RetryPeriod.Duration,
		Callbacks:     callbacks,
		WatchDog:      electionChecker,
		Name:          leaseName,
	})

	panic("unreachable")
}

// createInitializersFunc creates a initializersFunc that returns all initializer
//  with expected as the result after filtering through filterFunc.
func createInitializersFunc(filterFunc leadermigration.FilterFunc, expected leadermigration.FilterResult) ControllerInitializersFunc {
	return func() map[string]InitFunc {
		initializers := make(map[string]InitFunc)
		for name, initializer := range NewControllerInitializers() {
			if filterFunc(name) == expected {
				initializers[name] = initializer
			}
		}
		return initializers
	}
}
