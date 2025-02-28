/*
based on sample-controller from https://github.com/kubernetes/sample-controller/blob/7047ee6ceceef2118a2017bbfff4a86c1f56f1ca/controller.go
*/

package runctl

import (
	"fmt"
	"log"
	"time"

	api "github.com/SAP/stewardci-core/pkg/apis/steward/v1alpha1"
	listers "github.com/SAP/stewardci-core/pkg/client/listers/steward/v1alpha1"
	"github.com/SAP/stewardci-core/pkg/k8s"
	"github.com/SAP/stewardci-core/pkg/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const kind = "PipelineRuns"

// Controller processes PipelineRun resources
type Controller struct {
	factory              k8s.ClientFactory
	pipelineRunFetcher   k8s.PipelineRunFetcher
	pipelineRunSynced    cache.InformerSynced
	pipelineRunLister    listers.PipelineRunLister
	tektonTaskRunsSynced cache.InformerSynced
	workqueue            workqueue.RateLimitingInterface
	metrics              metrics.Metrics
}

// NewController creates new Controller
func NewController(factory k8s.ClientFactory, pipelineRunFetcher k8s.PipelineRunFetcher, metrics metrics.Metrics) *Controller {
	pipelineRunInformer := factory.StewardInformerFactory().Steward().V1alpha1().PipelineRuns()
	tektonTaskRunInformer := factory.TektonInformerFactory().Tekton().V1alpha1().TaskRuns()
	controller := &Controller{
		factory:              factory,
		pipelineRunFetcher:   pipelineRunFetcher,
		pipelineRunSynced:    pipelineRunInformer.Informer().HasSynced,
		pipelineRunLister:    pipelineRunInformer.Lister(),
		tektonTaskRunsSynced: tektonTaskRunInformer.Informer().HasSynced,
		workqueue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), kind),
		metrics:              metrics,
	}
	pipelineRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.addPipelineRun,
		UpdateFunc: func(old, new interface{}) {
			controller.addPipelineRun(new)
		},
	})
	tektonTaskRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleTektonTaskRun,
		UpdateFunc: func(old, new interface{}) {
			controller.handleTektonTaskRun(new)
		},
	})
	return controller
}

// Run runs the controller
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	log.Printf("Sync cache")
	if ok := cache.WaitForCacheSync(stopCh, c.pipelineRunSynced, c.tektonTaskRunsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	log.Printf("Start workers")
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}
	log.Printf("Workers running")
	<-stopCh
	log.Printf("Workers stopped")
	return nil
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		log.Printf("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) changeState(pipelineRun k8s.PipelineRun, state api.State) error {
	oldState, err := pipelineRun.UpdateState(state)
	if err != nil {
		return err
	}
	if oldState != nil {
		err = c.metrics.ObserveDurationByState(oldState)
		if err != nil {
			log.Printf("Faild to measure state '%+v': '%s'", oldState, err)
		}
	}
	return nil
}

func (c *Controller) createRunManager(pipelineRun k8s.PipelineRun) RunManager {
	tenant := k8s.NewTenantNamespace(c.factory, pipelineRun.GetNamespace())
	workFactory := tenant.TargetClientFactory()
	namespaceManager := k8s.NewNamespaceManager(c.factory, runNamespacePrefix, runNamespaceRandomLength)
	return NewRunManager(workFactory, tenant, namespaceManager)
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Foo resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	pipelineRun, err := c.pipelineRunFetcher.ByKey(key)
	if err != nil {
		return err
	}

	// If pipelineRun is not found there is nothing to sync
	if pipelineRun == nil {
		return nil
	}

	// Check if object has deletion timestamp
	// If not, try to add finalizer if missing
	if pipelineRun.HasDeletionTimestamp() {
		runManager := c.createRunManager(pipelineRun)
		err = runManager.Cleanup(pipelineRun)
		if err == nil {
			pipelineRun.DeleteFinalizerIfExists()
		}
		return err
	}
	pipelineRun.AddFinalizer()

	// Check if pipeline run is killed or completed
	if c.skipKilledOrCompleted(pipelineRun) {
		return nil
	}

	runManager := c.createRunManager(pipelineRun)
	// Process pipeline run based on current state
	switch state := pipelineRun.GetStatus().State; state {
	// TODO fix #117
	// Runs might be left in state `preparing` after a controller crash.
	// Those must be recovered.
	case api.StateUndefined:
		c.changeState(pipelineRun, api.StatePreparing)
		err = runManager.Start(pipelineRun)
		if err != nil {
			pipelineRun.StoreErrorAsMessage(err, "error syncing resource")
			c.changeState(pipelineRun, api.StateCleaning)
			return nil
		}
		c.metrics.CountStart()
		c.changeState(pipelineRun, api.StateWaiting)
	case api.StateWaiting:
		run, err := runManager.GetRun(pipelineRun)
		if err != nil {
			pipelineRun.StoreErrorAsMessage(err, "error syncing resource")
			c.changeState(pipelineRun, api.StateCleaning)
			return nil
		}
		started := run.GetStartTime()
		if started != nil {
			c.changeState(pipelineRun, api.StateRunning)
		}
	case api.StateRunning:
		run, err := runManager.GetRun(pipelineRun)
		if err != nil {
			pipelineRun.StoreErrorAsMessage(err, "error syncing resource")
			c.changeState(pipelineRun, api.StateCleaning)
			return nil
		}
		containerInfo := run.GetContainerInfo()
		pipelineRun.UpdateContainer(containerInfo)
		if finished, result := run.IsFinished(); finished {
			var msg string
			if containerInfo != nil && containerInfo.Terminated != nil {
				msg = containerInfo.Terminated.Message
			}
			if msg == "" {
				cond := run.GetSucceededCondition()
				if cond != nil {
					msg = cond.Message
				}
			}
			pipelineRun.UpdateMessage(msg)
			pipelineRun.UpdateResult(result)
			c.changeState(pipelineRun, api.StateCleaning)
			c.metrics.CountResult(result)
		}
	case api.StateCleaning:
		err = runManager.Cleanup(pipelineRun)
		if err == nil {
			c.changeState(pipelineRun, api.StateFinished)
		}
		return err
	default:
		log.Printf("Skip PipelineRun with state %s", pipelineRun.GetStatus().State)
	}
	return nil
}

// skipKilledOrCompleted checks if pipeline run is killed or completed.
func (c *Controller) skipKilledOrCompleted(pipelineRun k8s.PipelineRun) bool {
	intent := pipelineRun.GetSpec().Intent
	if intent == api.IntentKill {
		switch result := pipelineRun.GetStatus().Result; result {
		case api.ResultUndefined:
			pipelineRun.UpdateMessage("Killed by user")
			pipelineRun.UpdateResult(api.ResultKilled)
			c.changeState(pipelineRun, api.StateCleaning)
			return true
		case api.ResultKilled:
			return true
		default:
			message := "Cannot kill completed pipeline run"
			if !(message == pipelineRun.GetStatus().Message) {
				pipelineRun.UpdateMessage(message)
			}
			return true
		}
	}
	return false
}

func (c *Controller) addPipelineRun(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	log.Printf("Add to workqueue '%s'", key)
	c.workqueue.Add(key)
}

// handleTektonTaskRun takes any resource implementing metav1.Object and attempts
// to find the PipelineRun resource that 'owns' it. It does this by looking for
// a specific annotation. If such annotation exists, the named PipelineRun
// is put into the controller's work queue to be processed.
func (c *Controller) handleTektonTaskRun(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		log.Printf("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	log.Printf("Processing object: %s", object.GetSelfLink())
	annotations := object.GetAnnotations()
	runKey := annotations[annotationPipelineRunKey]
	if runKey != "" {
		log.Printf("Add to workqueue '%s'", runKey)
		c.workqueue.Add(runKey)
	}
}
