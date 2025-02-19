// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package shoot

import (
	"context"
	"fmt"
	"sync"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap/keys"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/features"
	"github.com/gardener/gardener/pkg/gardenlet/apis/config"
	confighelper "github.com/gardener/gardener/pkg/gardenlet/apis/config/helper"
	gardenletfeatures "github.com/gardener/gardener/pkg/gardenlet/features"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	runtimecache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Controller controls Shoots.
type Controller struct {
	clientMap   clientmap.ClientMap
	gardenCache runtimecache.Cache
	logger      logrus.FieldLogger

	config                     *config.GardenletConfiguration
	shootReconciler            reconcile.Reconciler
	careReconciler             reconcile.Reconciler
	seedRegistrationReconciler reconcile.Reconciler
	migrationReconciler        reconcile.Reconciler

	shootQueue            workqueue.RateLimitingInterface
	shootSeedQueue        workqueue.RateLimitingInterface
	shootCareQueue        workqueue.RateLimitingInterface
	seedRegistrationQueue workqueue.RateLimitingInterface
	shootMigrationQueue   workqueue.RateLimitingInterface

	hasSyncedFuncs         []cache.InformerSynced
	numberOfRunningWorkers int
	workerCh               chan int
}

// NewShootController takes a Kubernetes client for the Garden clusters <k8sGardenClient>, a struct
// holding information about the acting Gardener, a <shootInformer>, and a <recorder> for
// event recording. It creates a new Gardener controller.
func NewShootController(
	ctx context.Context,
	clientMap clientmap.ClientMap,
	logger logrus.FieldLogger,
	config *config.GardenletConfiguration,
	identity *gardencorev1beta1.Gardener,
	gardenClusterIdentity string,
	imageVector imagevector.ImageVector,
	recorder record.EventRecorder,
) (*Controller, error) {
	gardenClient, err := clientMap.GetClient(ctx, keys.ForGarden())
	if err != nil {
		return nil, err
	}

	shootInformer, err := gardenClient.Cache().GetInformer(ctx, &gardencorev1beta1.Shoot{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Shoot Informer: %w", err)
	}

	seedInformer, err := gardenClient.Cache().GetInformer(ctx, &gardencorev1beta1.Seed{})
	if err != nil {
		return nil, fmt.Errorf("could not get Seed informer: %w", err)
	}

	shootController := &Controller{
		clientMap:   clientMap,
		gardenCache: gardenClient.Cache(),
		logger:      logger,

		config:                     config,
		shootReconciler:            NewShootReconciler(clientMap, recorder, logger, imageVector, identity, gardenClusterIdentity, config),
		careReconciler:             NewCareReconciler(clientMap, logger, imageVector, identity, gardenClusterIdentity, config),
		seedRegistrationReconciler: NewSeedRegistrationReconciler(clientMap, recorder, logger),
		migrationReconciler:        NewMigrationReconciler(clientMap, logger, config),

		shootCareQueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "shoot-care"),
		shootQueue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "shoot"),
		shootSeedQueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "shoot-seeds"),
		seedRegistrationQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "shooted-seed-registration"),
		shootMigrationQueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "shoot-migration"),

		workerCh: make(chan int),
	}

	shootInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controllerutils.ShootFilterFunc(confighelper.SeedNameFromSeedConfig(config.SeedConfig)),
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { shootController.shootAdd(ctx, obj, false) },
			UpdateFunc: func(oldObj, newObj interface{}) { shootController.shootUpdate(ctx, oldObj, newObj) },
			DeleteFunc: func(obj interface{}) { shootController.shootDelete(ctx, obj) },
		},
	})

	shootInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controllerutils.ShootFilterFunc(confighelper.SeedNameFromSeedConfig(config.SeedConfig)),
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    shootController.shootCareAdd,
			UpdateFunc: shootController.shootCareUpdate,
		},
	})

	shootInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controllerutils.ShootFilterFunc(confighelper.SeedNameFromSeedConfig(config.SeedConfig)),
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    shootController.seedRegistrationAdd,
			UpdateFunc: shootController.seedRegistrationUpdate,
		},
	})

	if gardenletfeatures.FeatureGate.Enabled(features.ForceRestore) && confighelper.OwnerChecksEnabledInSeedConfig(config.SeedConfig) {
		shootInformer.AddEventHandler(cache.FilteringResourceEventHandler{
			FilterFunc: controllerutils.ShootMigrationFilterFunc(ctx, gardenClient.Cache(), confighelper.SeedNameFromSeedConfig(config.SeedConfig)),
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    shootController.shootMigrationAdd,
				UpdateFunc: shootController.shootMigrationUpdate,
				DeleteFunc: shootController.shootMigrationDelete,
			},
		})
	}

	shootController.hasSyncedFuncs = []cache.InformerSynced{
		shootInformer.HasSynced,
		seedInformer.HasSynced,
	}

	return shootController, nil
}

// Run runs the Controller until the given stop channel can be read from.
func (c *Controller) Run(ctx context.Context, shootWorkers, shootCareWorkers, shootMigrationWorkers int) {
	var waitGroup sync.WaitGroup

	if !cache.WaitForCacheSync(ctx.Done(), c.hasSyncedFuncs...) {
		c.logger.Error("Timed out waiting for caches to sync")
		return
	}

	// Count number of running workers.
	go func() {
		for res := range c.workerCh {
			c.numberOfRunningWorkers += res
			c.logger.Debugf("Current number of running Shoot workers is %d", c.numberOfRunningWorkers)
		}
	}()

	gardenClient, err := c.clientMap.GetClient(ctx, keys.ForGarden())
	if err != nil {
		panic(fmt.Errorf("failed to get garden client: %v", err))
	}

	// Update Shoots before starting the workers.
	shootFilterFunc := controllerutils.ShootFilterFunc(confighelper.SeedNameFromSeedConfig(c.config.SeedConfig))
	shoots := &gardencorev1beta1.ShootList{}
	if err := c.gardenCache.List(ctx, shoots); err != nil {
		c.logger.Errorf("Failed to fetch shoots resources: %v", err.Error())
		return
	}
	for _, shoot := range shoots.Items {
		if !shootFilterFunc(&shoot) {
			continue
		}

		// Check if the status indicates that an operation is processing and mark it as "aborted".
		if shoot.Status.LastOperation != nil && shoot.Status.LastOperation.State == gardencorev1beta1.LastOperationStateProcessing {
			newShoot := shoot.DeepCopy()
			newShoot.Status.LastOperation.State = gardencorev1beta1.LastOperationStateAborted
			if err := gardenClient.Client().Status().Update(ctx, newShoot); err != nil {
				panic(fmt.Sprintf("Failed to update shoot status [%s]: %v ", client.ObjectKeyFromObject(newShoot).String(), err.Error()))
			}
		}
	}

	c.logger.Info("Shoot controller initialized.")

	for i := 0; i < shootWorkers; i++ {
		controllerutils.CreateWorker(ctx, c.shootQueue, "Shoot", c.shootReconciler, &waitGroup, c.workerCh)
	}
	for i := 0; i < shootCareWorkers; i++ {
		controllerutils.CreateWorker(ctx, c.shootCareQueue, "Shoot Care", c.careReconciler, &waitGroup, c.workerCh)
	}
	for i := 0; i < shootWorkers/2+1; i++ {
		controllerutils.CreateWorker(ctx, c.shootSeedQueue, "Shooted Seeds Reconciliation", c.shootReconciler, &waitGroup, c.workerCh)
		controllerutils.CreateWorker(ctx, c.seedRegistrationQueue, "Shooted Seeds Registration", c.seedRegistrationReconciler, &waitGroup, c.workerCh)
	}
	if gardenletfeatures.FeatureGate.Enabled(features.ForceRestore) && confighelper.OwnerChecksEnabledInSeedConfig(c.config.SeedConfig) {
		for i := 0; i < shootMigrationWorkers; i++ {
			controllerutils.CreateWorker(ctx, c.shootMigrationQueue, "Shoot Migration", c.migrationReconciler, &waitGroup, c.workerCh)
		}
	}

	// Shutdown handling
	<-ctx.Done()
	c.seedRegistrationQueue.ShutDown()
	c.shootCareQueue.ShutDown()
	c.shootQueue.ShutDown()
	c.shootSeedQueue.ShutDown()
	c.shootMigrationQueue.ShutDown()

	for {
		var (
			seedRegistrationQueueLength = c.seedRegistrationQueue.Len()
			shootQueueLength            = c.shootQueue.Len()
			shootCareQueueLength        = c.shootCareQueue.Len()
			shootSeedQueueLength        = c.shootSeedQueue.Len()
			shootMigrationQueueLength   = c.shootMigrationQueue.Len()
			queueLengths                = shootQueueLength + shootCareQueueLength + shootSeedQueueLength + seedRegistrationQueueLength + shootMigrationQueueLength
		)
		if queueLengths == 0 && c.numberOfRunningWorkers == 0 {
			c.logger.Debug("No running Shoot worker and no items left in the queues. Terminated Shoot controller...")
			break
		}
		c.logger.Debugf("Waiting for %d Shoot worker(s) to finish (%d item(s) left in the queues)...", c.numberOfRunningWorkers, queueLengths)
		time.Sleep(5 * time.Second)
	}

	waitGroup.Wait()
}

func (c *Controller) getShootQueue(ctx context.Context, obj interface{}) workqueue.RateLimitingInterface {
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if shoot, ok := obj.(*gardencorev1beta1.Shoot); ok && c.shootIsSeed(timeoutCtx, shoot) {
		return c.shootSeedQueue
	}
	return c.shootQueue
}

func (c *Controller) shootIsSeed(ctx context.Context, shoot *gardencorev1beta1.Shoot) bool {
	managedSeed, err := kutil.GetManagedSeedWithReader(ctx, c.gardenCache, shoot.Namespace, shoot.Name)
	if err != nil {
		return false
	}
	return managedSeed != nil
}
