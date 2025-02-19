// Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package botanist_test

import (
	"context"
	"fmt"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardencorev1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	seedmanagementv1alpha1 "github.com/gardener/gardener/pkg/apis/seedmanagement/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	fakeclientset "github.com/gardener/gardener/pkg/client/kubernetes/fake"
	"github.com/gardener/gardener/pkg/features"
	gardenletfeatures "github.com/gardener/gardener/pkg/gardenlet/features"
	mockclient "github.com/gardener/gardener/pkg/mock/controller-runtime/client"
	"github.com/gardener/gardener/pkg/operation"
	. "github.com/gardener/gardener/pkg/operation/botanist"
	"github.com/gardener/gardener/pkg/operation/botanist/component"
	"github.com/gardener/gardener/pkg/operation/botanist/component/etcd"
	mocketcd "github.com/gardener/gardener/pkg/operation/botanist/component/etcd/mock"
	seedpkg "github.com/gardener/gardener/pkg/operation/seed"
	shootpkg "github.com/gardener/gardener/pkg/operation/shoot"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/test"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	hvpav1alpha1 "github.com/gardener/hvpa-controller/api/v1alpha1"
	"github.com/golang/mock/gomock"
	"github.com/hashicorp/go-multierror"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Etcd", func() {
	var (
		ctrl             *gomock.Controller
		kubernetesClient kubernetes.Interface
		c                *mockclient.MockClient
		reader           *mockclient.MockReader
		botanist         *Botanist

		ctx                   = context.TODO()
		fakeErr               = fmt.Errorf("fake err")
		namespace             = "shoot--foo--bar"
		role                  = "test"
		class                 = etcd.ClassImportant
		maintenanceTimeWindow = gardencorev1beta1.MaintenanceTimeWindow{
			Begin: "123456+0000",
			End:   "162543+0000",
		}
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		c = mockclient.NewMockClient(ctrl)
		reader = mockclient.NewMockReader(ctrl)
		kubernetesClient = fakeclientset.NewClientSetBuilder().
			WithClient(c).
			WithAPIReader(reader).
			Build()
		botanist = &Botanist{Operation: &operation.Operation{}}
	})

	AfterEach(func() {
		ctrl.Finish()
	})

	Describe("#DefaultEtcd", func() {
		var hvpaEnabled = true

		BeforeEach(func() {
			botanist.K8sSeedClient = kubernetesClient
			botanist.Seed = &seedpkg.Seed{}
			botanist.Shoot = &shootpkg.Shoot{
				SeedNamespace: namespace,
			}
			botanist.Seed.SetInfo(&gardencorev1beta1.Seed{})
			botanist.Shoot.SetInfo(&gardencorev1beta1.Shoot{
				Spec: gardencorev1beta1.ShootSpec{
					Maintenance: &gardencorev1beta1.Maintenance{
						TimeWindow: &maintenanceTimeWindow,
					},
				},
			})
		})

		Context("no shooted seed", func() {
			BeforeEach(func() {
				botanist.ManagedSeed = nil
			})

			computeUpdateMode := func(class etcd.Class, purpose gardencorev1beta1.ShootPurpose) string {
				if class == etcd.ClassImportant && purpose == gardencorev1beta1.ShootPurposeProduction {
					return hvpav1alpha1.UpdateModeOff
				}
				return hvpav1alpha1.UpdateModeMaintenanceWindow
			}

			for _, etcdClass := range []etcd.Class{etcd.ClassNormal, etcd.ClassImportant} {
				for _, shootPurpose := range []gardencorev1beta1.ShootPurpose{gardencorev1beta1.ShootPurposeEvaluation, gardencorev1beta1.ShootPurposeProduction} {
					var (
						class   = etcdClass
						purpose = shootPurpose
					)
					It(fmt.Sprintf("should successfully create an etcd interface: class = %q, purpose = %q", class, purpose), func() {
						defer test.WithFeatureGate(gardenletfeatures.FeatureGate, features.HVPA, hvpaEnabled)()

						botanist.Shoot.Purpose = purpose

						validator := &newEtcdValidator{
							expectedClient:                  Equal(c),
							expectedLogger:                  BeNil(),
							expectedNamespace:               Equal(namespace),
							expectedRole:                    Equal(role),
							expectedClass:                   Equal(class),
							expectedRetainReplicas:          BeFalse(),
							expectedStorageCapacity:         Equal("10Gi"),
							expectedDefragmentationSchedule: Equal(pointer.String("34 12 */3 * *")),
							expectedHVPAConfig: Equal(&etcd.HVPAConfig{
								Enabled:               hvpaEnabled,
								MaintenanceTimeWindow: maintenanceTimeWindow,
								ScaleDownUpdateMode:   pointer.String(computeUpdateMode(class, purpose)),
							}),
						}

						oldNewEtcd := NewEtcd
						defer func() { NewEtcd = oldNewEtcd }()
						NewEtcd = validator.NewEtcd

						etcd, err := botanist.DefaultEtcd(role, class)
						Expect(etcd).NotTo(BeNil())
						Expect(err).NotTo(HaveOccurred())
					})
				}
			}
		})

		Context("no HVPAShootedSeed feature gate", func() {
			hvpaForShootedSeedEnabled := false

			BeforeEach(func() {
				botanist.ManagedSeed = &seedmanagementv1alpha1.ManagedSeed{}
			})

			It("should successfully create an etcd interface (normal class)", func() {
				defer test.WithFeatureGate(gardenletfeatures.FeatureGate, features.HVPAForShootedSeed, hvpaForShootedSeedEnabled)()

				validator := &newEtcdValidator{
					expectedClient:                  Equal(c),
					expectedLogger:                  BeNil(),
					expectedNamespace:               Equal(namespace),
					expectedRole:                    Equal(role),
					expectedClass:                   Equal(class),
					expectedRetainReplicas:          BeFalse(),
					expectedStorageCapacity:         Equal("10Gi"),
					expectedDefragmentationSchedule: Equal(pointer.String("34 12 * * *")),
					expectedHVPAConfig: Equal(&etcd.HVPAConfig{
						Enabled:               hvpaForShootedSeedEnabled,
						MaintenanceTimeWindow: maintenanceTimeWindow,
						ScaleDownUpdateMode:   pointer.String(hvpav1alpha1.UpdateModeMaintenanceWindow),
					}),
				}

				oldNewEtcd := NewEtcd
				defer func() { NewEtcd = oldNewEtcd }()
				NewEtcd = validator.NewEtcd

				etcd, err := botanist.DefaultEtcd(role, class)
				Expect(etcd).NotTo(BeNil())
				Expect(err).NotTo(HaveOccurred())
			})

			It("should successfully create an etcd interface (important class)", func() {
				class := etcd.ClassImportant

				defer test.WithFeatureGate(gardenletfeatures.FeatureGate, features.HVPAForShootedSeed, hvpaForShootedSeedEnabled)()

				validator := &newEtcdValidator{
					expectedClient:                  Equal(c),
					expectedLogger:                  BeNil(),
					expectedNamespace:               Equal(namespace),
					expectedRole:                    Equal(role),
					expectedClass:                   Equal(class),
					expectedRetainReplicas:          BeFalse(),
					expectedStorageCapacity:         Equal("10Gi"),
					expectedDefragmentationSchedule: Equal(pointer.String("34 12 * * *")),
					expectedHVPAConfig: Equal(&etcd.HVPAConfig{
						Enabled:               hvpaForShootedSeedEnabled,
						MaintenanceTimeWindow: maintenanceTimeWindow,
						ScaleDownUpdateMode:   pointer.String(hvpav1alpha1.UpdateModeMaintenanceWindow),
					}),
				}

				oldNewEtcd := NewEtcd
				defer func() { NewEtcd = oldNewEtcd }()
				NewEtcd = validator.NewEtcd

				etcd, err := botanist.DefaultEtcd(role, class)
				Expect(etcd).NotTo(BeNil())
				Expect(err).NotTo(HaveOccurred())
			})
		})

		It("should return an error because the maintenance time window cannot be parsed", func() {
			defer test.WithFeatureGate(gardenletfeatures.FeatureGate, features.HVPA, true)()
			botanist.Shoot.GetInfo().Spec.Maintenance.TimeWindow = &gardencorev1beta1.MaintenanceTimeWindow{
				Begin: "foobar",
				End:   "barfoo",
			}

			etcd, err := botanist.DefaultEtcd(role, class)
			Expect(etcd).To(BeNil())
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("#DeployEtcd", func() {
		var (
			etcdMain, etcdEvents *mocketcd.MockInterface

			secretNameCA     = "ca-etcd"
			secretNameServer = "etcd-server-cert"
			secretNameClient = "etcd-client-tls"
			checksumCA       = "1234"
			checksumServer   = "5678"
			checksumClient   = "9012"
			shootUID         = types.UID("uuid")
		)

		BeforeEach(func() {
			etcdMain, etcdEvents = mocketcd.NewMockInterface(ctrl), mocketcd.NewMockInterface(ctrl)

			botanist.K8sSeedClient = kubernetesClient
			botanist.StoreCheckSum(secretNameCA, checksumCA)
			botanist.StoreCheckSum(secretNameServer, checksumServer)
			botanist.StoreCheckSum(secretNameClient, checksumClient)
			botanist.Seed = &seedpkg.Seed{}
			botanist.Shoot = &shootpkg.Shoot{
				Components: &shootpkg.Components{
					ControlPlane: &shootpkg.ControlPlane{
						EtcdMain:   etcdMain,
						EtcdEvents: etcdEvents,
					},
				},
				SeedNamespace:         namespace,
				BackupEntryName:       namespace + "--" + string(shootUID),
				InternalClusterDomain: "internal.example.com",
			}
			botanist.Seed.SetInfo(&gardencorev1beta1.Seed{
				Status: gardencorev1beta1.SeedStatus{
					ClusterIdentity: pointer.String("seed-identity"),
				},
			})
			botanist.Shoot.SetInfo(&gardencorev1beta1.Shoot{
				Spec: gardencorev1beta1.ShootSpec{
					Maintenance: &gardencorev1beta1.Maintenance{
						TimeWindow: &maintenanceTimeWindow,
					},
				},
				Status: gardencorev1beta1.ShootStatus{
					TechnicalID: namespace,
					UID:         shootUID,
				},
			})

			etcdMain.EXPECT().SetSecrets(etcd.Secrets{
				CA:     component.Secret{Name: secretNameCA, Checksum: checksumCA},
				Server: component.Secret{Name: secretNameServer, Checksum: checksumServer},
				Client: component.Secret{Name: secretNameClient, Checksum: checksumClient},
			})
			etcdEvents.EXPECT().SetSecrets(etcd.Secrets{
				CA:     component.Secret{Name: secretNameCA, Checksum: checksumCA},
				Server: component.Secret{Name: secretNameServer, Checksum: checksumServer},
				Client: component.Secret{Name: secretNameClient, Checksum: checksumClient},
			})
		})

		It("should fail when the deploy function fails for etcd-main", func() {
			etcdMain.EXPECT().Deploy(ctx).Return(fakeErr)
			etcdEvents.EXPECT().Deploy(ctx)

			err := botanist.DeployEtcd(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err).To(BeAssignableToTypeOf(&multierror.Error{}))
			Expect(err.(*multierror.Error).Errors).To(ConsistOf(Equal(fakeErr)))
		})

		It("should fail when the deploy function fails for etcd-events", func() {
			etcdMain.EXPECT().Deploy(ctx)
			etcdEvents.EXPECT().Deploy(ctx).Return(fakeErr)

			err := botanist.DeployEtcd(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err).To(BeAssignableToTypeOf(&multierror.Error{}))
			Expect(err.(*multierror.Error).Errors).To(ConsistOf(Equal(fakeErr)))
		})

		Context("w/o backup", func() {
			BeforeEach(func() {
				botanist.Seed.GetInfo().Spec.Backup = nil
			})

			It("should set the secrets and deploy", func() {
				etcdMain.EXPECT().Deploy(ctx)
				etcdEvents.EXPECT().Deploy(ctx)
				Expect(botanist.DeployEtcd(ctx)).To(Succeed())
			})
		})

		Context("w/ backup", func() {
			var (
				backupProvider = "prov"
				bucketName     = "container"
				backupSecret   = &corev1.Secret{
					Data: map[string][]byte{
						"bucketName": []byte(bucketName),
					},
				}

				expectGetBackupSecret = func() {
					c.EXPECT().Get(ctx, kutil.Key(namespace, "etcd-backup"), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
						func(ctx context.Context, key client.ObjectKey, obj client.Object) error {
							backupSecret.DeepCopyInto(obj.(*corev1.Secret))
							return nil
						},
					)
				}
				expectSetBackupConfig = func() {
					etcdMain.EXPECT().SetBackupConfig(&etcd.BackupConfig{
						Provider:             backupProvider,
						SecretRefName:        "etcd-backup",
						Prefix:               namespace + "--" + string(shootUID),
						Container:            bucketName,
						FullSnapshotSchedule: "1 12 * * *",
					})
				}
				expectSetOwnerCheckConfig = func() {
					etcdMain.EXPECT().SetOwnerCheckConfig(&etcd.OwnerCheckConfig{
						Name: "owner.internal.example.com",
						ID:   "seed-identity",
					})
				}
			)

			BeforeEach(func() {
				botanist.Seed.GetInfo().Spec.Backup = &gardencorev1beta1.SeedBackup{
					Provider: backupProvider,
				}
			})

			It("should set the secrets and deploy with owner checks", func() {
				defer test.WithFeatureGate(gardenletfeatures.FeatureGate, features.UseDNSRecords, true)()

				expectGetBackupSecret()
				expectSetBackupConfig()
				expectSetOwnerCheckConfig()
				etcdMain.EXPECT().Deploy(ctx)
				etcdEvents.EXPECT().Deploy(ctx)

				Expect(botanist.DeployEtcd(ctx)).To(Succeed())
			})

			It("should set the secrets and deploy without owner checks if they are disabled", func() {
				defer test.WithFeatureGate(gardenletfeatures.FeatureGate, features.UseDNSRecords, true)()
				botanist.Seed.GetInfo().Spec.Settings = &gardencorev1beta1.SeedSettings{
					OwnerChecks: &gardencorev1beta1.SeedSettingOwnerChecks{
						Enabled: false,
					},
				}

				expectGetBackupSecret()
				expectSetBackupConfig()
				etcdMain.EXPECT().Deploy(ctx)
				etcdEvents.EXPECT().Deploy(ctx)

				Expect(botanist.DeployEtcd(ctx)).To(Succeed())
			})

			It("should set the secrets and deploy without owner checks if the UseDNSRecords feature gate is disabled", func() {
				defer test.WithFeatureGate(gardenletfeatures.FeatureGate, features.UseDNSRecords, false)()

				expectGetBackupSecret()
				expectSetBackupConfig()
				etcdMain.EXPECT().Deploy(ctx)
				etcdEvents.EXPECT().Deploy(ctx)

				Expect(botanist.DeployEtcd(ctx)).To(Succeed())
			})

			It("should fail when reading the backup secret fails", func() {
				c.EXPECT().Get(ctx, kutil.Key(namespace, "etcd-backup"), gomock.AssignableToTypeOf(&corev1.Secret{})).Return(fakeErr)

				Expect(botanist.DeployEtcd(ctx)).To(MatchError(fakeErr))
			})

			It("should fail when the backup schedule cannot be determined", func() {
				botanist.Shoot.GetInfo().Spec.Maintenance.TimeWindow = &gardencorev1beta1.MaintenanceTimeWindow{
					Begin: "foobar",
					End:   "barfoo",
				}
				expectGetBackupSecret()

				Expect(botanist.DeployEtcd(ctx)).To(HaveOccurred())
			})
		})
	})

	Describe("#DestroyEtcd", func() {
		var (
			etcdMain, etcdEvents *mocketcd.MockInterface
		)

		BeforeEach(func() {
			etcdMain, etcdEvents = mocketcd.NewMockInterface(ctrl), mocketcd.NewMockInterface(ctrl)

			botanist.Shoot = &shootpkg.Shoot{
				Components: &shootpkg.Components{
					ControlPlane: &shootpkg.ControlPlane{
						EtcdMain:   etcdMain,
						EtcdEvents: etcdEvents,
					},
				},
			}
		})

		It("should fail when the destroy function fails for etcd-main", func() {
			etcdMain.EXPECT().Destroy(ctx).Return(fakeErr)
			etcdEvents.EXPECT().Destroy(ctx)

			err := botanist.DestroyEtcd(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err).To(BeAssignableToTypeOf(&multierror.Error{}))
			Expect(err.(*multierror.Error).Errors).To(ConsistOf(Equal(fakeErr)))
		})

		It("should fail when the destroy function fails for etcd-events", func() {
			etcdMain.EXPECT().Destroy(ctx)
			etcdEvents.EXPECT().Destroy(ctx).Return(fakeErr)

			err := botanist.DestroyEtcd(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err).To(BeAssignableToTypeOf(&multierror.Error{}))
			Expect(err.(*multierror.Error).Errors).To(ConsistOf(Equal(fakeErr)))
		})

		It("should succeed when both etcd-main and etcd-events destroy is successful", func() {
			etcdMain.EXPECT().Destroy(ctx)
			etcdEvents.EXPECT().Destroy(ctx)

			Expect(botanist.DestroyEtcd(ctx)).To(Succeed())
		})
	})

	Describe("#ScaleETCDTo*", func() {
		var (
			etcdMain, etcdEvents *druidv1alpha1.Etcd
			replicas             int
		)

		JustBeforeEach(func() {
			etcdEvents = &druidv1alpha1.Etcd{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "etcd-events",
					Namespace: namespace,
				},
				Spec: druidv1alpha1.EtcdSpec{
					Replicas: replicas,
				},
			}
			etcdMain = &druidv1alpha1.Etcd{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "etcd-main",
					Namespace: namespace,
				},
				Spec: druidv1alpha1.EtcdSpec{
					Replicas: replicas,
				},
			}
			botanist.K8sSeedClient = kubernetesClient
			botanist.Shoot = &shootpkg.Shoot{SeedNamespace: namespace}
		})

		Describe("#ScaleETCDToZero", func() {
			BeforeEach(func() {
				replicas = 1
			})

			It("should scale both etcds to 0", func() {
				now := time.Date(100, 1, 1, 0, 0, 0, 0, time.UTC)
				nowFunc := func() time.Time {
					return now
				}
				defer test.WithVar(&kubernetes.TimeNow, nowFunc)()

				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdMain), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = *etcdMain
						return nil
					})
				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdEvents), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = *etcdEvents
						return nil
					})
				c.EXPECT().Patch(ctx, gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{}), gomock.Any()).DoAndReturn(
					func(_ context.Context, etcd *druidv1alpha1.Etcd, patch client.Patch, _ ...client.PatchOption) error {
						data, err := patch.Data(etcd)
						Expect(err).ToNot(HaveOccurred())
						Expect(string(data)).To(Equal(`{"metadata":{"annotations":{"gardener.cloud/operation":"reconcile","gardener.cloud/timestamp":"0100-01-01 00:00:00 +0000 UTC"}},"spec":{"replicas":0}}`))
						return nil
					}).Times(2)

				Expect(botanist.ScaleETCDToZero(ctx)).To(Succeed())
			})

			It("should return the error when scaling etcd-events fails", func() {
				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdEvents), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = *etcdEvents
						return nil
					})

				etcdEvents.Annotations = map[string]string{
					gardencorev1beta1constants.GardenerOperation: gardencorev1beta1constants.GardenerOperationReconcile,
				}
				etcdEvents.Spec.Replicas = 0
				c.EXPECT().Patch(ctx, etcdEvents, gomock.Any()).Return(fakeErr)

				Expect(botanist.ScaleETCDToZero(ctx)).To(MatchError(fakeErr))
			})

			It("should return the error when scaling etcd-main fails", func() {
				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdEvents), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = *etcdEvents
						return nil
					})

				etcdEvents.Annotations = map[string]string{
					gardencorev1beta1constants.GardenerOperation: gardencorev1beta1constants.GardenerOperationReconcile,
				}
				etcdEvents.Spec.Replicas = 0
				c.EXPECT().Patch(ctx, etcdEvents, gomock.Any())

				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdMain), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = druidv1alpha1.Etcd{
							ObjectMeta: etcdMain.ObjectMeta,
							Spec: druidv1alpha1.EtcdSpec{
								Replicas: 1,
							},
						}
						return nil
					})

				etcdMain.Annotations = map[string]string{
					gardencorev1beta1constants.GardenerOperation: gardencorev1beta1constants.GardenerOperationReconcile,
				}
				etcdMain.Spec.Replicas = 0
				c.EXPECT().Patch(ctx, etcdMain, gomock.Any()).Return(fakeErr)

				Expect(botanist.ScaleETCDToZero(ctx)).To(MatchError(fakeErr))
			})
		})

		Describe("#ScaleETCDToOne", func() {
			BeforeEach(func() {
				replicas = 0
			})

			It("should scale both etcds to 1", func() {
				now := time.Date(100, 1, 1, 0, 0, 0, 0, time.UTC)
				nowFunc := func() time.Time {
					return now
				}
				defer test.WithVar(&kubernetes.TimeNow, nowFunc)()

				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdMain), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = *etcdMain
						return nil
					})
				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdEvents), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = *etcdEvents
						return nil
					})
				c.EXPECT().Patch(ctx, gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{}), gomock.Any()).DoAndReturn(
					func(_ context.Context, etcd *druidv1alpha1.Etcd, patch client.Patch, _ ...client.PatchOption) error {
						data, err := patch.Data(etcd)
						Expect(err).ToNot(HaveOccurred())
						Expect(string(data)).To(Equal(`{"metadata":{"annotations":{"gardener.cloud/operation":"reconcile","gardener.cloud/timestamp":"0100-01-01 00:00:00 +0000 UTC"}},"spec":{"replicas":1}}`))
						return nil
					}).Times(2)

				Expect(botanist.ScaleETCDToOne(ctx)).To(Succeed())
			})

			It("should return the error when scaling etcd-events fails", func() {
				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdEvents), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = *etcdEvents
						return nil
					})

				etcdEvents.Annotations = map[string]string{
					gardencorev1beta1constants.GardenerOperation: gardencorev1beta1constants.GardenerOperationReconcile,
				}
				etcdEvents.Spec.Replicas = 1
				c.EXPECT().Patch(ctx, etcdEvents, gomock.Any()).Return(fakeErr)

				Expect(botanist.ScaleETCDToOne(ctx)).To(MatchError(fakeErr))
			})

			It("should return the error when scaling etcd-main fails", func() {
				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdEvents), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = *etcdEvents
						return nil
					})

				etcdEvents.Annotations = map[string]string{
					gardencorev1beta1constants.GardenerOperation: gardencorev1beta1constants.GardenerOperationReconcile,
				}
				etcdEvents.Spec.Replicas = 1
				c.EXPECT().Patch(ctx, etcdEvents, gomock.Any())

				c.EXPECT().Get(ctx, client.ObjectKeyFromObject(etcdMain), gomock.AssignableToTypeOf(&druidv1alpha1.Etcd{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, etcd *druidv1alpha1.Etcd) error {
						*etcd = druidv1alpha1.Etcd{
							ObjectMeta: etcdMain.ObjectMeta,
							Spec: druidv1alpha1.EtcdSpec{
								Replicas: 1,
							},
						}
						return nil
					})

				etcdMain.Annotations = map[string]string{
					gardencorev1beta1constants.GardenerOperation: gardencorev1beta1constants.GardenerOperationReconcile,
				}
				etcdMain.Spec.Replicas = 1
				c.EXPECT().Patch(ctx, etcdMain, gomock.Any()).Return(fakeErr)

				Expect(botanist.ScaleETCDToOne(ctx)).To(MatchError(fakeErr))
			})
		})
	})
})

type newEtcdValidator struct {
	etcd.Interface

	expectedClient                  gomegatypes.GomegaMatcher
	expectedLogger                  gomegatypes.GomegaMatcher
	expectedNamespace               gomegatypes.GomegaMatcher
	expectedRole                    gomegatypes.GomegaMatcher
	expectedClass                   gomegatypes.GomegaMatcher
	expectedRetainReplicas          gomegatypes.GomegaMatcher
	expectedStorageCapacity         gomegatypes.GomegaMatcher
	expectedDefragmentationSchedule gomegatypes.GomegaMatcher
	expectedHVPAConfig              gomegatypes.GomegaMatcher
}

func (v *newEtcdValidator) NewEtcd(
	client client.Client,
	logger logrus.FieldLogger,
	namespace string,
	role string,
	class etcd.Class,
	retainReplicas bool,
	storageCapacity string,
	defragmentationSchedule *string,
) etcd.Interface {
	Expect(client).To(v.expectedClient)
	Expect(logger).To(v.expectedLogger)
	Expect(namespace).To(v.expectedNamespace)
	Expect(role).To(v.expectedRole)
	Expect(class).To(v.expectedClass)
	Expect(retainReplicas).To(v.expectedRetainReplicas)
	Expect(storageCapacity).To(v.expectedStorageCapacity)
	Expect(defragmentationSchedule).To(v.expectedDefragmentationSchedule)

	return v
}

func (v *newEtcdValidator) SetHVPAConfig(config *etcd.HVPAConfig) {
	Expect(config).To(v.expectedHVPAConfig)
}
