/*
Copyright 2016 The Kubernetes Authors.

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

package server

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/admission"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilversion "k8s.io/apiserver/pkg/util/version"
	"k8s.io/component-base/featuregate"
	baseversion "k8s.io/component-base/version"
	netutils "k8s.io/utils/net"
	"sigs.k8s.io/apiserver-runtime/internal/sample-apiserver/pkg/admission/wardleinitializer"
	"sigs.k8s.io/apiserver-runtime/internal/sample-apiserver/pkg/apiserver"
	clientset "sigs.k8s.io/apiserver-runtime/internal/sample-apiserver/pkg/generated/clientset/versioned"
	informers "sigs.k8s.io/apiserver-runtime/internal/sample-apiserver/pkg/generated/informers/externalversions"
)

const defaultEtcdPathPrefix = "/registry/wardle.example.com"

// WardleServerOptions contains state for master/api server
type WardleServerOptions struct {
	RecommendedOptions *genericoptions.RecommendedOptions

	SharedInformerFactory informers.SharedInformerFactory
	StdOut                io.Writer
	StdErr                io.Writer

	AlternateDNS []string
}

func WardleVersionToKubeVersion(ver *version.Version) *version.Version {
	if ver.Major() != 1 {
		return nil
	}
	kubeVer := utilversion.DefaultKubeEffectiveVersion().BinaryVersion()
	// "1.2" maps to kubeVer
	offset := int(ver.Minor()) - 2
	mappedVer := kubeVer.OffsetMinor(offset)
	if mappedVer.GreaterThan(kubeVer) {
		return kubeVer
	}
	return mappedVer
}

// NewWardleServerOptions returns a new WardleServerOptions
func NewWardleServerOptions(out, errOut io.Writer, versions ...schema.GroupVersion) *WardleServerOptions {
	o := &WardleServerOptions{
		RecommendedOptions: genericoptions.NewRecommendedOptions(
			defaultEtcdPathPrefix,
			// change: apiserver-runtime
			apiserver.Codecs.LegacyCodec(versions...),
		),

		StdOut: out,
		StdErr: errOut,
	}
	// change: apiserver-runtime
	//o.RecommendedOptions.Etcd.StorageConfig.EncodeVersioner = runtime.NewMultiGroupVersioner(v1alpha1.SchemeGroupVersion, schema.GroupKind{Group: v1alpha1.GroupName})
	o.RecommendedOptions.Etcd.StorageConfig.EncodeVersioner = schema.GroupVersions(versions)
	return o
}

// NewCommandStartWardleServer provides a CLI handler for 'start master' command
// with a default WardleServerOptions.
func NewCommandStartWardleServer(ctx context.Context, defaults *WardleServerOptions) *cobra.Command {
	o := *defaults
	cmd := &cobra.Command{
		Short: "Launch a wardle API server",
		Long:  "Launch a wardle API server",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return utilversion.DefaultComponentGlobalsRegistry.Set()
		},
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Complete(); err != nil {
				return err
			}
			if err := o.Validate(args); err != nil {
				return err
			}
			if err := o.RunWardleServer(c.Context()); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.SetContext(ctx)

	flags := cmd.Flags()
	o.RecommendedOptions.AddFlags(flags)

	// The following lines demonstrate how to configure version compatibility and feature gates
	// for the "Wardle" component, as an example of KEP-4330.

	// Create an effective version object for the "Wardle" component.
	// This initializes the binary version, the emulation version and the minimum compatibility version.
	//
	// Note:
	// - The binary version represents the actual version of the running source code.
	// - The emulation version is the version whose capabilities are being emulated by the binary.
	// - The minimum compatibility version specifies the minimum version that the component remains compatible with.
	//
	// Refer to KEP-4330 for more details: https://github.com/kubernetes/enhancements/blob/master/keps/sig-architecture/4330-compatibility-versions
	defaultWardleVersion := "1.2"
	// Register the "Wardle" component with the global component registry,
	// associating it with its effective version and feature gate configuration.
	// Will skip if the component has been registered, like in the integration test.
	_, wardleFeatureGate := utilversion.DefaultComponentGlobalsRegistry.ComponentGlobalsOrRegister(
		apiserver.WardleComponentName, utilversion.NewEffectiveVersion(defaultWardleVersion),
		featuregate.NewVersionedFeatureGate(version.MustParse(defaultWardleVersion)))

	// Add versioned feature specifications for the "BanFlunder" feature.
	// These specifications, together with the effective version, determine if the feature is enabled.
	utilruntime.Must(wardleFeatureGate.AddVersioned(map[featuregate.Feature]featuregate.VersionedSpecs{
		"BanFlunder": {
			{Version: version.MustParse("1.2"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
			{Version: version.MustParse("1.1"), Default: true, PreRelease: featuregate.Beta},
			{Version: version.MustParse("1.0"), Default: false, PreRelease: featuregate.Alpha},
		},
	}))

	// Register the default kube component if not already present in the global registry.
	_, _ = utilversion.DefaultComponentGlobalsRegistry.ComponentGlobalsOrRegister(utilversion.DefaultKubeComponent,
		utilversion.NewEffectiveVersion(baseversion.DefaultKubeBinaryVersion), utilfeature.DefaultMutableFeatureGate)

	// Set the emulation version mapping from the "Wardle" component to the kube component.
	// This ensures that the emulation version of the latter is determined by the emulation version of the former.
	utilruntime.Must(utilversion.DefaultComponentGlobalsRegistry.SetEmulationVersionMapping(apiserver.WardleComponentName, utilversion.DefaultKubeComponent, WardleVersionToKubeVersion))

	utilversion.DefaultComponentGlobalsRegistry.AddFlags(flags)

	return cmd
}

// Validate validates WardleServerOptions
func (o WardleServerOptions) Validate(args []string) error {
	errors := []error{}
	errors = append(errors, o.RecommendedOptions.Validate()...)
	errors = append(errors, utilversion.DefaultComponentGlobalsRegistry.Validate()...)
	return utilerrors.NewAggregate(errors)
}

// Complete fills in fields required to have valid data
func (o *WardleServerOptions) Complete() error {
	// change: apiserver-runtime
	//if utilversion.DefaultComponentGlobalsRegistry.FeatureGateFor(apiserver.WardleComponentName).Enabled("BanFlunder") {
	//	// register admission plugins
	//	banflunder.Register(o.RecommendedOptions.Admission.Plugins)
	//
	//	// add admission plugins to the RecommendedPluginOrder
	//	o.RecommendedOptions.Admission.RecommendedPluginOrder = append(o.RecommendedOptions.Admission.RecommendedPluginOrder, "BanFlunder")
	//}

	ApplyServerOptionsFns(o)
	return nil
}

// Config returns config for the api server given WardleServerOptions
func (o *WardleServerOptions) Config() (*apiserver.Config, error) {
	// TODO have a "real" external address
	if err := o.RecommendedOptions.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", o.AlternateDNS, []net.IP{netutils.ParseIPSloppy("127.0.0.1")}); err != nil {
		return nil, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	o.RecommendedOptions.ExtraAdmissionInitializers = func(c *genericapiserver.RecommendedConfig) ([]admission.PluginInitializer, error) {
		client, err := clientset.NewForConfig(c.LoopbackClientConfig)
		if err != nil {
			return nil, err
		}
		informerFactory := informers.NewSharedInformerFactory(client, c.LoopbackClientConfig.Timeout)
		o.SharedInformerFactory = informerFactory
		return []admission.PluginInitializer{wardleinitializer.New(informerFactory)}, nil
	}

	serverConfig := genericapiserver.NewRecommendedConfig(apiserver.Codecs)
	serverConfig = ApplyRecommendedConfigFns(serverConfig)

	// change: apiserver-runtime
	//serverConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(sampleopenapi.GetOpenAPIDefinitions, openapi.NewDefinitionNamer(apiserver.Scheme))
	//serverConfig.OpenAPIConfig.Info.Title = "Wardle"
	//serverConfig.OpenAPIConfig.Info.Version = "0.1"
	//
	//serverConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(sampleopenapi.GetOpenAPIDefinitions, openapi.NewDefinitionNamer(apiserver.Scheme))
	//serverConfig.OpenAPIV3Config.Info.Title = "Wardle"
	//serverConfig.OpenAPIV3Config.Info.Version = "0.1"

	serverConfig.FeatureGate = utilversion.DefaultComponentGlobalsRegistry.FeatureGateFor(utilversion.DefaultKubeComponent)
	serverConfig.EffectiveVersion = utilversion.DefaultComponentGlobalsRegistry.EffectiveVersionFor(apiserver.WardleComponentName)

	if err := o.RecommendedOptions.ApplyTo(serverConfig); err != nil {
		return nil, err
	}

	// change: apiserver-runtime
	serverConfig = ApplyRecommendedConfigFns(serverConfig)

	config := &apiserver.Config{
		GenericConfig: serverConfig,
		ExtraConfig:   apiserver.ExtraConfig{},
	}
	return config, nil
}

// RunWardleServer starts a new WardleServer given WardleServerOptions
func (o WardleServerOptions) RunWardleServer(ctx context.Context) error {
	config, err := o.Config()
	if err != nil {
		return err
	}

	server, err := config.Complete().New()
	if err != nil {
		return err
	}

	server.GenericAPIServer.AddPostStartHookOrDie("start-sample-server-informers", func(context genericapiserver.PostStartHookContext) error {
		config.GenericConfig.SharedInformerFactory.Start(context.Done())
		o.SharedInformerFactory.Start(context.Done())
		return nil
	})

	return server.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}
