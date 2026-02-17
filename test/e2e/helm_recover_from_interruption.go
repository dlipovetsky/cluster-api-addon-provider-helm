//go:build e2e
// +build e2e

/*
Copyright 2024 The Kubernetes Authors.

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

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	helmAction "helm.sh/helm/v3/pkg/action"
	helmRelease "helm.sh/helm/v3/pkg/release"
	helmDriver "helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	capi_e2e "sigs.k8s.io/cluster-api/test/e2e"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/cluster-api/util"

	addonsv1alpha1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
)

const (
	metallbChartRepoURL = "https://metallb.github.io/metallb"
	metallbChartName    = "metallb"
	metallbVersion1     = "0.15.1"
	metallbVersion2     = "0.15.2"
	recoveryReleaseName = "metallb-release"
	recoveryReleaseNs   = "metallb-namespace"
)

var metallbRecoveryValues = `prometheus:
  scrapeAnnotations: false`

var _ = Describe("Recover release from stuck status", func() {
	var (
		ctx               = context.Background()
		specName          = "helm-recovery"
		namespace         *corev1.Namespace
		cancelWatches     context.CancelFunc
		result            *clusterctl.ApplyClusterTemplateAndWaitResult
		clusterName       string
		clusterNamePrefix string
		additionalCleanup func()
		specTimes         = map[string]time.Time{}
	)

	BeforeEach(func() {
		logCheckpoint(specTimes)

		Expect(ctx).NotTo(BeNil(), "ctx is required for %s spec", specName)
		Expect(e2eConfig).NotTo(BeNil(), "Invalid argument. e2eConfig can't be nil when calling %s spec", specName)
		Expect(clusterctlConfigPath).To(BeAnExistingFile(), "Invalid argument. clusterctlConfigPath must be an existing file when calling %s spec", specName)
		Expect(bootstrapClusterProxy).NotTo(BeNil(), "Invalid argument. bootstrapClusterProxy can't be nil when calling %s spec", specName)
		Expect(os.MkdirAll(artifactFolder, 0o755)).To(Succeed(), "Invalid argument. artifactFolder can't be created for %s spec", specName)
		Expect(e2eConfig.Variables).To(HaveKey(capi_e2e.KubernetesVersion))

		clusterNameSpace := os.Getenv("CLUSTER_NAMESPACE")
		if clusterNameSpace == "" {
			clusterNamePrefix = fmt.Sprintf("caaph-recovery-%s", util.RandomString(6))
		} else {
			clusterNamePrefix = clusterNameSpace
		}

		var err error
		namespace, cancelWatches, err = setupSpecNamespace(ctx, clusterNamePrefix, bootstrapClusterProxy, artifactFolder)
		Expect(err).NotTo(HaveOccurred())

		result = new(clusterctl.ApplyClusterTemplateAndWaitResult)
		additionalCleanup = nil
	})

	AfterEach(func() {
		if result.Cluster == nil {
			_ = bootstrapClusterProxy.GetClient().Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace.Name}, result.Cluster)
		}

		CheckTestBeforeCleanup()

		defer func() {
			cancelWatches()
		}()

		Logf("Dumping all the Cluster API resources in the %q namespace", namespace.Name)
		framework.DumpAllResources(ctx, framework.DumpAllResourcesInput{
			Lister:               bootstrapClusterProxy.GetClient(),
			KubeConfigPath:       bootstrapClusterProxy.GetKubeconfigPath(),
			ClusterctlConfigPath: clusterctlConfigPath,
			Namespace:            namespace.Name,
			LogPath:              filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName(), "resources"),
		})

		if result.Cluster != nil && !skipLogCollection {
			Byf("Dumping logs from the %q workload cluster", result.Cluster.Name)
			bootstrapClusterProxy.CollectWorkloadClusterLogs(ctx, result.Cluster.Namespace, result.Cluster.Name, filepath.Join(artifactFolder, "clusters", result.Cluster.Name))
		}

		if !skipCleanup {
			Logf("Deleting all clusters in the %s namespace", namespace.Name)
			framework.DeleteAllClustersAndWait(ctx, framework.DeleteAllClustersAndWaitInput{
				ClusterProxy:         bootstrapClusterProxy,
				ClusterctlConfigPath: clusterctlConfigPath,
				Namespace:            namespace.Name,
			}, e2eConfig.GetIntervals(specName, "wait-delete-cluster")...)
			Logf("Deleting namespace used for hosting the %q test spec", specName)
			framework.DeleteNamespace(ctx, framework.DeleteNamespaceInput{
				Deleter: bootstrapClusterProxy.GetClient(),
				Name:    namespace.Name,
			})
		}

		if additionalCleanup != nil {
			Logf("Running additional cleanup for the %q test spec", specName)
			additionalCleanup()
		}

		logCheckpoint(specTimes)
	})

	recoveryHelmChartProxy := func() *addonsv1alpha1.HelmChartProxy {
		return &addonsv1alpha1.HelmChartProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "metallb",
				Namespace: namespace.Name,
			},
			Spec: addonsv1alpha1.HelmChartProxySpec{
				ClusterSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"MetalLBChart": "enabled",
					},
				},
				ReleaseName:       recoveryReleaseName,
				ReleaseNamespace:  recoveryReleaseNs,
				ChartName:         metallbChartName,
				RepoURL:           metallbChartRepoURL,
				Version:           metallbVersion1,
				ValuesTemplate:    metallbRecoveryValues,
				ReconcileStrategy: string(addonsv1alpha1.ReconcileStrategyContinuous),
			},
		}
	}

	Context("Creating workload cluster [REQUIRED]", func() {
		It("Recover release from uninstalling status", func() {
			clusterName = fmt.Sprintf("%s-%s", specName, util.RandomString(6))
			clusterctl.ApplyClusterTemplateAndWait(ctx, createApplyClusterTemplateInput(
				specName,
				withNamespace(namespace.Name),
				withClusterName(clusterName),
				withControlPlaneMachineCount(1),
				withWorkerMachineCount(1),
				withControlPlaneWaiters(clusterctl.ControlPlaneWaiters{
					WaitForControlPlaneInitialized: EnsureControlPlaneInitialized,
				}),
			), result)

			hcp := recoveryHelmChartProxy()
			Expect(bootstrapClusterProxy.GetClient().Create(ctx, hcp)).To(Succeed())

			By("Installing metallb chart v0.15.1")
			EnsureHelmReleaseInstallOrUpgrade(ctx, specName, bootstrapClusterProxy, &HelmInstallInput{
				BootstrapClusterProxy: bootstrapClusterProxy,
				Namespace:             namespace,
				ClusterName:           clusterName,
				HelmChartProxy:        hcp,
			}, nil, true)

			hrp, err := getHelmReleaseProxy(ctx, bootstrapClusterProxy.GetClient(), clusterName, *hcp)
			Expect(err).NotTo(HaveOccurred())

			workloadClusterProxy := bootstrapClusterProxy.GetWorkloadCluster(ctx, namespace.Name, clusterName)
			Expect(workloadClusterProxy).NotTo(BeNil())

			By("Creating a new release with status uninstalling (copy of latest)")
			CreateHelmReleaseWithStatusFromLatest(ctx, workloadClusterProxy, hrp.Spec.ReleaseNamespace, hrp.Spec.ReleaseName, helmRelease.StatusUninstalling)

			By("Uninstalling the release")
			HelmUninstallSpec(ctx, func() HelmUninstallInput {
				return HelmUninstallInput{
					BootstrapClusterProxy: bootstrapClusterProxy,
					Namespace:             namespace,
					ClusterName:           clusterName,
					HelmChartProxy:        hcp,
				}
			})

			By("Verifying release is uninstalled")
			actionConfig := getHelmActionConfigForTests(ctx, workloadClusterProxy, hrp.Spec.ReleaseNamespace)
			Eventually(func() error {
				getClient := helmAction.NewGet(actionConfig)
				r, err := getClient.Run(hrp.Spec.ReleaseName)
				if err != nil {
					if err == helmDriver.ErrReleaseNotFound {
						return nil
					}
					return err
				}
				return fmt.Errorf("Helm release %s still exists", r.Name)
			}, e2eConfig.GetIntervals(specName, "wait-helm-release")...).Should(Succeed())
		})

		It("Recover release from pending-install status", func() {
			clusterName = fmt.Sprintf("%s-%s", specName, util.RandomString(6))
			clusterctl.ApplyClusterTemplateAndWait(ctx, createApplyClusterTemplateInput(
				specName,
				withNamespace(namespace.Name),
				withClusterName(clusterName),
				withControlPlaneMachineCount(1),
				withWorkerMachineCount(1),
				withControlPlaneWaiters(clusterctl.ControlPlaneWaiters{
					WaitForControlPlaneInitialized: EnsureControlPlaneInitialized,
				}),
			), result)

			hcp := recoveryHelmChartProxy()
			Expect(bootstrapClusterProxy.GetClient().Create(ctx, hcp)).To(Succeed())

			By("Installing metallb chart v0.15.1")
			EnsureHelmReleaseInstallOrUpgrade(ctx, specName, bootstrapClusterProxy, &HelmInstallInput{
				BootstrapClusterProxy: bootstrapClusterProxy,
				Namespace:             namespace,
				ClusterName:           clusterName,
				HelmChartProxy:        hcp,
			}, nil, true)

			hrp, err := getHelmReleaseProxy(ctx, bootstrapClusterProxy.GetClient(), clusterName, *hcp)
			Expect(err).NotTo(HaveOccurred())

			workloadClusterProxy := bootstrapClusterProxy.GetWorkloadCluster(ctx, namespace.Name, clusterName)
			Expect(workloadClusterProxy).NotTo(BeNil())

			By("Setting release status to pending-install")
			SetHelmReleaseStatus(ctx, workloadClusterProxy, hrp.Spec.ReleaseNamespace, hrp.Spec.ReleaseName, helmRelease.StatusPendingInstall)

			By("Waiting for controller to recover release to deployed")
			releaseWaitInput := GetWaitForHelmReleaseDeployedInput(ctx, workloadClusterProxy, hrp.Spec.ReleaseName, hrp.Spec.ReleaseNamespace, specName)
			WaitForHelmReleaseDeployed(ctx, releaseWaitInput, e2eConfig.GetIntervals(specName, "wait-helm-release-deployed")...)
		})

		It("Recover release from pending-upgrade status", func() {
			clusterName = fmt.Sprintf("%s-%s", specName, util.RandomString(6))
			clusterctl.ApplyClusterTemplateAndWait(ctx, createApplyClusterTemplateInput(
				specName,
				withNamespace(namespace.Name),
				withClusterName(clusterName),
				withControlPlaneMachineCount(1),
				withWorkerMachineCount(1),
				withControlPlaneWaiters(clusterctl.ControlPlaneWaiters{
					WaitForControlPlaneInitialized: EnsureControlPlaneInitialized,
				}),
			), result)

			hcp := recoveryHelmChartProxy()
			Expect(bootstrapClusterProxy.GetClient().Create(ctx, hcp)).To(Succeed())

			By("Installing metallb chart v0.15.1")
			EnsureHelmReleaseInstallOrUpgrade(ctx, specName, bootstrapClusterProxy, &HelmInstallInput{
				BootstrapClusterProxy: bootstrapClusterProxy,
				Namespace:             namespace,
				ClusterName:           clusterName,
				HelmChartProxy:        hcp,
			}, nil, true)

			hrp, err := getHelmReleaseProxy(ctx, bootstrapClusterProxy.GetClient(), clusterName, *hcp)
			Expect(err).NotTo(HaveOccurred())

			workloadClusterProxy := bootstrapClusterProxy.GetWorkloadCluster(ctx, namespace.Name, clusterName)
			Expect(workloadClusterProxy).NotTo(BeNil())

			By("Creating a new release with status pending-upgrade (copy of latest)")
			CreateHelmReleaseWithStatusFromLatest(ctx, workloadClusterProxy, hrp.Spec.ReleaseNamespace, hrp.Spec.ReleaseName, helmRelease.StatusPendingUpgrade)

			By("Upgrading release to v0.15.2")
			hcp.Spec.Version = metallbVersion2
			Expect(bootstrapClusterProxy.GetClient().Update(ctx, hcp)).To(Succeed())
			EnsureHelmReleaseInstallOrUpgrade(ctx, specName, bootstrapClusterProxy, nil, &HelmUpgradeInput{
				BootstrapClusterProxy: bootstrapClusterProxy,
				Namespace:             namespace,
				ClusterName:           clusterName,
				HelmChartProxy:        hcp,
				ExpectedRevision:      2,
			}, true)

			By("Verifying release is at v0.15.2")
			releaseWaitInput := GetWaitForHelmReleaseDeployedInput(ctx, workloadClusterProxy, hrp.Spec.ReleaseName, hrp.Spec.ReleaseNamespace, specName)
			release := WaitForHelmReleaseDeployed(ctx, releaseWaitInput, e2eConfig.GetIntervals(specName, "wait-helm-release-deployed")...)
			Expect(release.Chart.Metadata.Version).To(Equal(metallbVersion2))
		})

		It("Recover release from pending-rollback status", func() {
			clusterName = fmt.Sprintf("%s-%s", specName, util.RandomString(6))
			clusterctl.ApplyClusterTemplateAndWait(ctx, createApplyClusterTemplateInput(
				specName,
				withNamespace(namespace.Name),
				withClusterName(clusterName),
				withControlPlaneMachineCount(1),
				withWorkerMachineCount(1),
				withControlPlaneWaiters(clusterctl.ControlPlaneWaiters{
					WaitForControlPlaneInitialized: EnsureControlPlaneInitialized,
				}),
			), result)

			hcp := recoveryHelmChartProxy()
			Expect(bootstrapClusterProxy.GetClient().Create(ctx, hcp)).To(Succeed())

			By("Installing metallb chart v0.15.1")
			EnsureHelmReleaseInstallOrUpgrade(ctx, specName, bootstrapClusterProxy, &HelmInstallInput{
				BootstrapClusterProxy: bootstrapClusterProxy,
				Namespace:             namespace,
				ClusterName:           clusterName,
				HelmChartProxy:        hcp,
			}, nil, true)

			By("Upgrading to v0.15.2")
			hcp.Spec.Version = metallbVersion2
			Expect(bootstrapClusterProxy.GetClient().Update(ctx, hcp)).To(Succeed())
			EnsureHelmReleaseInstallOrUpgrade(ctx, specName, bootstrapClusterProxy, nil, &HelmUpgradeInput{
				BootstrapClusterProxy: bootstrapClusterProxy,
				Namespace:             namespace,
				ClusterName:           clusterName,
				HelmChartProxy:        hcp,
				ExpectedRevision:      2,
			}, true)

			hrp, err := getHelmReleaseProxy(ctx, bootstrapClusterProxy.GetClient(), clusterName, *hcp)
			Expect(err).NotTo(HaveOccurred())

			workloadClusterProxy := bootstrapClusterProxy.GetWorkloadCluster(ctx, namespace.Name, clusterName)
			Expect(workloadClusterProxy).NotTo(BeNil())

			By("Creating a new release with status pending-rollback (copy of latest)")
			CreateHelmReleaseWithStatusFromLatest(ctx, workloadClusterProxy, hrp.Spec.ReleaseNamespace, hrp.Spec.ReleaseName, helmRelease.StatusPendingRollback)

			By("Rolling back to v0.15.1 via HelmChartProxy version change")
			hcp.Spec.Version = metallbVersion1
			Expect(bootstrapClusterProxy.GetClient().Update(ctx, hcp)).To(Succeed())
			EnsureHelmReleaseInstallOrUpgrade(ctx, specName, bootstrapClusterProxy, nil, &HelmUpgradeInput{
				BootstrapClusterProxy: bootstrapClusterProxy,
				Namespace:             namespace,
				ClusterName:           clusterName,
				HelmChartProxy:        hcp,
				ExpectedRevision:      3, // upgrade from 0.15.2 to 0.15.1 creates new revision
			}, true)

			By("Verifying release is at v0.15.1")
			releaseWaitInput := GetWaitForHelmReleaseDeployedInput(ctx, workloadClusterProxy, hrp.Spec.ReleaseName, hrp.Spec.ReleaseNamespace, specName)
			release := WaitForHelmReleaseDeployed(ctx, releaseWaitInput, e2eConfig.GetIntervals(specName, "wait-helm-release-deployed")...)
			Expect(release.Chart.Metadata.Version).To(Equal(metallbVersion1))
		})
	})
})
