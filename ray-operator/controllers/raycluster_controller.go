package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	rbacv1 "k8s.io/api/rbac/v1"

	rayiov1alpha1 "github.com/ray-project/kuberay/ray-operator/api/raycluster/v1alpha1"
	"github.com/ray-project/kuberay/ray-operator/controllers/common"
	_ "github.com/ray-project/kuberay/ray-operator/controllers/common"
	"github.com/ray-project/kuberay/ray-operator/controllers/utils"

	"k8s.io/client-go/tools/record"

	"github.com/go-logr/logr"
	_ "k8s.io/api/apps/v1beta1"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	log                       = logf.Log.WithName("raycluster-controller")
	DefaultRequeueDuration    = 2 * time.Second
	PrioritizeWorkersToDelete bool
)

// NewReconciler returns a new reconcile.Reconciler
func NewReconciler(mgr manager.Manager) *RayClusterReconciler {
	return &RayClusterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Log:      ctrl.Log.WithName("controllers").WithName("RayCluster"),
		Recorder: mgr.GetEventRecorderFor("raycluster-controller"),
	}
}

var _ reconcile.Reconciler = &RayClusterReconciler{}

// RayClusterReconciler reconciles a RayCluster object
type RayClusterReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile reads that state of the cluster for a RayCluster object and makes changes based on it
// and what is in the RayCluster.Spec
// Automatically generate RBAC rules to allow the Controller to read and write workloads
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/finalizer,verbs=update
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;create;update
// +kubebuilder:rbac:groups=core,resources=serviceaccount,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=roles,verbs=get;list;watch;create;delete;update
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;list;watch;create;delete
// Reconcile used to bridge the desired state with the current state
func (r *RayClusterReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	_ = r.Log.WithValues("raycluster", request.NamespacedName)
	log.Info("reconciling RayCluster", "cluster name", request.Name)

	// Fetch the RayCluster instance
	instance := &rayiov1alpha1.RayCluster{}
	if err := r.Get(context.TODO(), request.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Read request instance not found error!")
		} else {
			log.Error(err, "Read request instance error!")
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if instance.DeletionTimestamp != nil && !instance.DeletionTimestamp.IsZero() {
		log.Info("RayCluser is being deleted, just ignore", "cluster name", request.Name)
		return ctrl.Result{}, nil
	}
	if err := r.reconcileAutoscalerServiceAccount(instance); err != nil {
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}
	if err := r.reconcileAutoscalerRole(instance); err != nil {
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}
	if err := r.reconcileAutoscalerRoleBinding(instance); err != nil {
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}
	if err := r.reconcileIngress(instance); err != nil {
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}
	if err := r.reconcileServices(instance); err != nil {
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}
	if err := r.reconcilePods(instance); err != nil {
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}
	// update the status if needed
	if err := r.updateStatus(instance); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Update status not found error", "cluster name", request.Name)
		} else {
			log.Error(err, "Update status error", "cluster name", request.Name)
		}
	}
	return ctrl.Result{}, nil
}

func (r *RayClusterReconciler) reconcileIngress(instance *rayiov1alpha1.RayCluster) error {
	if instance.Spec.HeadGroupSpec.EnableIngress == nil || !*instance.Spec.HeadGroupSpec.EnableIngress {
		return nil
	}

	headIngresses := networkingv1.IngressList{}
	filterLabels := client.MatchingLabels{common.RayClusterLabelKey: instance.Name}
	if err := r.List(context.TODO(), &headIngresses, client.InNamespace(instance.Namespace), filterLabels); err != nil {
		return err
	}

	if headIngresses.Items != nil && len(headIngresses.Items) == 1 {
		r.Log.Info("reconcileIngresses", "head service ingress found", headIngresses.Items[0].Name)
		return nil
	}

	if headIngresses.Items == nil || len(headIngresses.Items) == 0 {
		ingress, err := common.BuildIngressForHeadService(*instance)
		if err != nil {
			return err
		}

		if err := controllerruntime.SetControllerReference(instance, ingress, r.Scheme); err != nil {
			return err
		}

		err = r.createHeadIngress(ingress, instance)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *RayClusterReconciler) reconcileServices(instance *rayiov1alpha1.RayCluster) error {
	headServices := corev1.ServiceList{}
	filterLabels := client.MatchingLabels{common.RayClusterLabelKey: instance.Name}
	if err := r.List(context.TODO(), &headServices, client.InNamespace(instance.Namespace), filterLabels); err != nil {
		return err
	}

	if headServices.Items != nil {
		if len(headServices.Items) == 1 {
			r.Log.Info("reconcileServices ", "head service found", headServices.Items[0].Name)
			// TODO: compare diff and reconcile the object
			// For example. ServiceType might be changed or port might be modified
			return nil
		}

		// This should never happen.
		// We add the protection here just in case controller has race issue or user manually create service with same label.
		if len(headServices.Items) > 1 {
			r.Log.Info("reconcileServices ", "Duplicates head service found", len(headServices.Items))
			return nil
		}
	}

	// Create head service if there's no existing one in the cluster.
	if headServices.Items == nil || len(headServices.Items) == 0 {
		rayHeadSvc, err := common.BuildServiceForHeadPod(*instance)
		if err != nil {
			return err
		}

		err = r.createHeadService(rayHeadSvc, instance)
		// if the service cannot be created we return the error and requeue
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *RayClusterReconciler) reconcilePods(instance *rayiov1alpha1.RayCluster) error {
	// check if all the pods exist
	headPods := corev1.PodList{}
	filterLabels := client.MatchingLabels{common.RayClusterLabelKey: instance.Name, common.RayNodeTypeLabelKey: string(rayiov1alpha1.HeadNode)}
	if err := r.List(context.TODO(), &headPods, client.InNamespace(instance.Namespace), filterLabels); err != nil {
		return err
	}
	// Reconcile head Pod
	if len(headPods.Items) == 1 {
		headPod := headPods.Items[0]
		log.Info("reconcilePods ", "head pod found", headPod.Name)
		if headPod.Status.Phase == v1.PodRunning || headPod.Status.Phase == v1.PodPending {
			log.Info("reconcilePods", "head pod is up and running... checking workers", headPod.Name)
		} else {
			return fmt.Errorf("head pod %s is not running nor pending", headPod.Name)
		}
	}
	if len(headPods.Items) == 0 || headPods.Items == nil {
		// create head pod
		log.Info("reconcilePods ", "creating head pod for cluster", instance.Name)
		if err := r.createHeadPod(*instance); err != nil {
			return err
		}
	} else if len(headPods.Items) > 1 {
		log.Info("reconcilePods ", "more than 1 head pod found for cluster", instance.Name)
		itemLength := len(headPods.Items)
		for index := 0; index < itemLength; index++ {
			if headPods.Items[index].Status.Phase == v1.PodRunning || headPods.Items[index].Status.Phase == v1.PodPending {
				// Remove the healthy pod  at index i from the list of pods to delete
				headPods.Items[index] = headPods.Items[len(headPods.Items)-1] // replace last element with the healthy head.
				headPods.Items = headPods.Items[:len(headPods.Items)-1]       // Truncate slice.
				itemLength--
			}
		}
		// delete all the extra head pod pods
		for _, extraHeadPodToDelete := range headPods.Items {
			if err := r.Delete(context.TODO(), &extraHeadPodToDelete); err != nil {
				return err
			}
		}
	}
	// Reconcile worker pods now
	for _, worker := range instance.Spec.WorkerGroupSpecs {
		workerPods := corev1.PodList{}
		filterLabels = client.MatchingLabels{common.RayClusterLabelKey: instance.Name, common.RayNodeGroupLabelKey: worker.GroupName}
		if err := r.List(context.TODO(), &workerPods, client.InNamespace(instance.Namespace), filterLabels); err != nil {
			return err
		}
		runningPods := corev1.PodList{}
		for _, aPod := range workerPods.Items {
			if (aPod.Status.Phase == v1.PodRunning || aPod.Status.Phase == v1.PodPending) && aPod.ObjectMeta.DeletionTimestamp == nil {
				runningPods.Items = append(runningPods.Items, aPod)
			}
		}
		r.updateLocalWorkersToDelete(&worker, runningPods.Items)
		diff := *worker.Replicas - int32(len(runningPods.Items))

		if PrioritizeWorkersToDelete {
			// Always remove the specified WorkersToDelete - regardless of the value of Replicas.
			// Essentially WorkersToDelete has to be deleted to meet the expectations of the Autoscaler.
			log.Info("reconcilePods", "removing the pods in the scaleStrategy of", worker.GroupName)
			for _, podsToDelete := range worker.ScaleStrategy.WorkersToDelete {
				pod := corev1.Pod{}
				pod.Name = podsToDelete
				pod.Namespace = utils.GetNamespace(instance.ObjectMeta)
				log.Info("Deleting pod", "namespace", pod.Namespace, "name", pod.Name)
				if err := r.Delete(context.TODO(), &pod); err != nil {
					if !errors.IsNotFound(err) {
						return err
					}
					log.Info("reconcilePods", "unable to delete worker ", pod.Name)
				} else {
					diff++
					r.Recorder.Eventf(instance, v1.EventTypeNormal, "Deleted", "Deleted pod %s", pod.Name)
				}
			}
			worker.ScaleStrategy.WorkersToDelete = []string{}
		}

		// Once we remove the feature flag and commit to those changes, the code below can be cleaned up
		// It will end being a simple: "if diff > 0 { } else { }"

		if diff > 0 {
			//pods need to be added
			log.Info("reconcilePods", "add workers for group", worker.GroupName)
			//create all workers of this group
			var i int32
			for i = 0; i < diff; i++ {
				log.Info("reconcilePods", "creating worker for group", worker.GroupName, fmt.Sprintf("index %d", i), fmt.Sprintf("in total %d", diff))
				if err := r.createWorkerPod(*instance, worker); err != nil {
					return err
				}
			}
		} else if diff == 0 {
			log.Info("reconcilePods", "all workers already exist for group", worker.GroupName)
			continue
		} else if -diff == int32(len(worker.ScaleStrategy.WorkersToDelete)) {
			log.Info("reconcilePods", "removing all the pods in the scaleStrategy of", worker.GroupName)
			for _, podsToDelete := range worker.ScaleStrategy.WorkersToDelete {
				pod := corev1.Pod{}
				pod.Name = podsToDelete
				pod.Namespace = utils.GetNamespace(instance.ObjectMeta)
				log.Info("Deleting pod", "namespace", pod.Namespace, "name", pod.Name)
				if err := r.Delete(context.TODO(), &pod); err != nil {
					if !errors.IsNotFound(err) {
						return err
					}
					log.Info("reconcilePods", "workers specified to delete was already deleted ", pod.Name)
				}
				r.Recorder.Eventf(instance, v1.EventTypeNormal, "Deleted", "Deleted pod %s", pod.Name)
			}
			continue
		} else {
			// diff < 0 and not the same absolute value as int32(len(worker.ScaleStrategy.WorkersToDelete)
			// we need to scale down
			workersToRemove := int32(len(runningPods.Items)) - *worker.Replicas
			randomlyRemovedWorkers := workersToRemove - int32(len(worker.ScaleStrategy.WorkersToDelete))
			// we only need to scale down the workers in the ScaleStrategy
			log.Info("reconcilePods", "removing all the pods in the scaleStrategy of", worker.GroupName)
			for _, podsToDelete := range worker.ScaleStrategy.WorkersToDelete {
				pod := corev1.Pod{}
				pod.Name = podsToDelete
				pod.Namespace = utils.GetNamespace(instance.ObjectMeta)
				log.Info("Deleting pod", "namespace", pod.Namespace, "name", pod.Name)
				if err := r.Delete(context.TODO(), &pod); err != nil {
					if !errors.IsNotFound(err) {
						return err
					}
					log.Info("reconcilePods", "workers specified to delete was already deleted ", pod.Name)
				}
				r.Recorder.Eventf(instance, v1.EventTypeNormal, "Deleted", "Deleted pod %s", pod.Name)
			}

			// remove the remaining pods not part of the scaleStrategy
			i := 0
			if int(randomlyRemovedWorkers) > 0 {
				for _, randomPodToDelete := range runningPods.Items {
					found := false
					for _, podsToDelete := range worker.ScaleStrategy.WorkersToDelete {
						if randomPodToDelete.Name == podsToDelete {
							found = true
							break
						}
					}
					if !found {
						log.Info("Randomly deleting pod ", "index ", i, "/", randomlyRemovedWorkers, "with name", randomPodToDelete.Name)
						if err := r.Delete(context.TODO(), &randomPodToDelete); err != nil {
							if !errors.IsNotFound(err) {
								return err
							}
							log.Info("reconcilePods", "workers specified to delete was already deleted ", randomPodToDelete.Name)
						}
						r.Recorder.Eventf(instance, v1.EventTypeNormal, "Deleted", "Deleted pod %s", randomPodToDelete.Name)
						// increment the number of deleted pods
						i++
						if i >= int(randomlyRemovedWorkers) {
							break
						}
					}
				}
			}
		}
	}
	return nil
}

func (r *RayClusterReconciler) updateLocalWorkersToDelete(worker *rayiov1alpha1.WorkerGroupSpec, runningItems []v1.Pod) {
	var actualWorkersToDelete []string
	itemMap := make(map[string]int)

	// Create a map for quick lookup.
	for _, item := range runningItems {
		itemMap[item.Name] = 1
	}

	// Build actualWorkersToDelete to only include running items.
	for _, workerToDelete := range worker.ScaleStrategy.WorkersToDelete {
		if _, ok := itemMap[workerToDelete]; ok {
			actualWorkersToDelete = append(actualWorkersToDelete, workerToDelete)
		}
	}

	worker.ScaleStrategy.WorkersToDelete = actualWorkersToDelete
}

func (r *RayClusterReconciler) createHeadIngress(ingress *networkingv1.Ingress, instance *rayiov1alpha1.RayCluster) error {
	// making sure the name is valid
	ingress.Name = utils.CheckName(ingress.Name)
	if err := controllerutil.SetControllerReference(instance, ingress, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(context.TODO(), ingress); err != nil {
		if errors.IsAlreadyExists(err) {
			log.Info("Ingress already exists,no need to create")
			return nil
		}
		log.Error(err, "Ingress create error!", "Ingress.Error", err)
		return err
	}
	log.Info("Ingress created successfully", "ingress name", ingress.Name)
	r.Recorder.Eventf(instance, v1.EventTypeNormal, "Created", "Created ingress %s", ingress.Name)
	return nil
}

func (r *RayClusterReconciler) createHeadService(rayHeadSvc *v1.Service, instance *rayiov1alpha1.RayCluster) error {
	// making sure the name is valid
	rayHeadSvc.Name = utils.CheckName(rayHeadSvc.Name)
	// Set controller reference
	if err := controllerutil.SetControllerReference(instance, rayHeadSvc, r.Scheme); err != nil {
		return err
	}

	if errSvc := r.Create(context.TODO(), rayHeadSvc); errSvc != nil {
		if errors.IsAlreadyExists(errSvc) {
			log.Info("Pod service already exist,no need to create")
			return nil
		}
		log.Error(errSvc, "Pod Service create error!", "Pod.Service.Error", errSvc)
		return errSvc
	}
	log.Info("Pod Service created successfully", "service name", rayHeadSvc.Name)
	r.Recorder.Eventf(instance, v1.EventTypeNormal, "Created", "Created service %s", rayHeadSvc.Name)
	return nil
}

func (r *RayClusterReconciler) createHeadPod(instance rayiov1alpha1.RayCluster) error {
	// build the pod then create it
	pod := r.buildHeadPod(instance)
	podIdentifier := types.NamespacedName{
		Name:      pod.Name,
		Namespace: pod.Namespace,
	}

	log.Info("createHeadPod", "head pod with name", pod.GenerateName)
	if err := r.Create(context.TODO(), &pod); err != nil {
		if errors.IsAlreadyExists(err) {
			fetchedPod := corev1.Pod{}
			// the pod might be in terminating state, we need to check
			if errPod := r.Get(context.TODO(), podIdentifier, &fetchedPod); errPod == nil {
				if fetchedPod.DeletionTimestamp != nil {
					log.Error(errPod, "create pod error!", "pod is in a terminating state, we will wait until it is cleaned up", podIdentifier)
					return err
				}
			}
			log.Info("Creating pod", "Pod already exists", pod.Name)
		} else {
			return err
		}
	}
	r.Recorder.Eventf(&instance, v1.EventTypeNormal, "Created", "Created head pod %s", pod.Name)
	return nil
}

func (r *RayClusterReconciler) createWorkerPod(instance rayiov1alpha1.RayCluster, worker rayiov1alpha1.WorkerGroupSpec) error {
	// build the pod then create it
	pod := r.buildWorkerPod(instance, worker)
	podIdentifier := types.NamespacedName{
		Name:      pod.Name,
		Namespace: pod.Namespace,
	}
	replica := corev1.Pod{}
	replica = pod
	if err := r.Create(context.TODO(), &replica); err != nil {
		if errors.IsAlreadyExists(err) {
			fetchedPod := corev1.Pod{}
			// the pod might be in terminating state, we need to check
			if errPod := r.Get(context.TODO(), podIdentifier, &fetchedPod); errPod == nil {
				if fetchedPod.DeletionTimestamp != nil {
					log.Error(errPod, "create pod error!", "pod is in a terminating state, we will wait until it is cleaned up", podIdentifier)
					return err
				}
			}
			log.Info("Creating pod", "Pod already exists", pod.Name)
		} else {
			log.Error(fmt.Errorf("createWorkerPod error"), "error creating pod", "pod", pod, "err = ", err)
			return err
		}
	}
	log.Info("Created pod", "Pod ", pod.GenerateName)
	r.Recorder.Eventf(&instance, v1.EventTypeNormal, "Created", "Created worker pod %s", pod.Name)
	return nil
}

// Build head instance pod(s).
func (r *RayClusterReconciler) buildHeadPod(instance rayiov1alpha1.RayCluster) corev1.Pod {
	podName := strings.ToLower(instance.Name + common.DashSymbol + string(rayiov1alpha1.HeadNode) + common.DashSymbol)
	podName = utils.CheckName(podName) // making sure the name is valid
	svcName := utils.GenerateServiceName(instance.Name)
	podConf := common.DefaultHeadPodTemplate(instance, instance.Spec.HeadGroupSpec, podName, svcName)
	pod := common.BuildPod(podConf, rayiov1alpha1.HeadNode, instance.Spec.HeadGroupSpec.RayStartParams, svcName)
	// Set raycluster instance as the owner and controller
	if err := controllerutil.SetControllerReference(&instance, &pod, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for raycluster pod")
	}

	return pod
}

// Build worker instance pods.
func (r *RayClusterReconciler) buildWorkerPod(instance rayiov1alpha1.RayCluster, worker rayiov1alpha1.WorkerGroupSpec) corev1.Pod {
	podName := strings.ToLower(instance.Name + common.DashSymbol + string(rayiov1alpha1.WorkerNode) + common.DashSymbol + worker.GroupName + common.DashSymbol)
	podName = utils.CheckName(podName) // making sure the name is valid
	svcName := utils.GenerateServiceName(instance.Name)
	podTemplateSpec := common.DefaultWorkerPodTemplate(instance, worker, podName, svcName)
	pod := common.BuildPod(podTemplateSpec, rayiov1alpha1.WorkerNode, worker.RayStartParams, svcName)
	// Set raycluster instance as the owner and controller
	if err := controllerutil.SetControllerReference(&instance, &pod, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for raycluster pod")
	}

	return pod
}

// SetupWithManager builds the reconciler.
func (r *RayClusterReconciler) SetupWithManager(mgr ctrl.Manager, reconcileConcurrency int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rayiov1alpha1.RayCluster{}).Named("raycluster-controller").
		Watches(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &rayiov1alpha1.RayCluster{},
		}).
		Watches(&source.Kind{Type: &corev1.Service{}}, &handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &rayiov1alpha1.RayCluster{},
		}).
		WithOptions(controller.Options{MaxConcurrentReconciles: reconcileConcurrency}).
		WithEventFilter(predicate.Funcs{
			CreateFunc:  r.CreateFunc,
			DeleteFunc:  r.DeleteFunc,
			UpdateFunc:  r.UpdateFunc,
			GenericFunc: r.GenericFunc,
		}).
		Complete(r)
}

func (r *RayClusterReconciler) CreateFunc(e event.CreateEvent) bool {
	log.Info(fmt.Sprintf("CREATE event:\n %+v\n", e.Object))
	return true
}

func (r *RayClusterReconciler) UpdateFunc(e event.UpdateEvent) bool {
	log.Info(fmt.Sprintf("UPDATE event: \n%+v -> \n%+v\n", e.ObjectOld, e.ObjectNew))
	return true
}

func (r *RayClusterReconciler) DeleteFunc(e event.DeleteEvent) bool {
	log.Info(fmt.Sprintf("DELETE event: \n%+v\n", e.Object))
	return true
}

func (r *RayClusterReconciler) GenericFunc(e event.GenericEvent) bool {
	log.Info(fmt.Sprintf("GENERIC event: \n%+v\n", e.Object))
	return true
}

func (r *RayClusterReconciler) updateStatus(instance *rayiov1alpha1.RayCluster) error {
	runtimePods := corev1.PodList{}
	filterLabels := client.MatchingLabels{"rayClusterName": instance.Name}
	if err := r.List(context.TODO(), &runtimePods, client.InNamespace(instance.Namespace), filterLabels); err != nil {
		return err
	}

	count := utils.CalculateAvailableReplicas(runtimePods)
	if instance.Status.AvailableWorkerReplicas != count {
		instance.Status.AvailableWorkerReplicas = count
	}

	count = utils.CalculateDesiredReplicas(instance)
	if instance.Status.DesiredWorkerReplicas != count {
		instance.Status.DesiredWorkerReplicas = count
	}

	count = utils.CalculateMinReplicas(instance)
	if instance.Status.MinWorkerReplicas != count {
		instance.Status.MinWorkerReplicas = count
	}

	count = utils.CalculateMaxReplicas(instance)
	if instance.Status.MaxWorkerReplicas != count {
		instance.Status.MaxWorkerReplicas = count
	}

	// TODO (@Jeffwan): Update state field later.
	// We always update instance no matter if there's one change or not.
	instance.Status.LastUpdateTime.Time = time.Now()
	if err := r.Status().Update(context.Background(), instance); err != nil {
		return err
	}

	return nil
}

func (r *RayClusterReconciler) reconcileAutoscalerServiceAccount(instance *rayiov1alpha1.RayCluster) error {
	if instance.Spec.EnableInTreeAutoscaling == nil || !*instance.Spec.EnableInTreeAutoscaling {
		return nil
	}

	serviceAccount := &corev1.ServiceAccount{}
	namespacedName := types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}
	if err := r.Get(context.TODO(), namespacedName, serviceAccount); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}

		// Create service account for autoscaler if there's no existing one in the cluster.
		serviceAccount, err := common.BuildServiceAccount(instance)
		if err != nil {
			return err
		}

		// making sure the name is valid
		serviceAccount.Name = utils.CheckName(serviceAccount.Name)
		// Set controller reference
		if err := controllerutil.SetControllerReference(instance, serviceAccount, r.Scheme); err != nil {
			return err
		}

		if errSvc := r.Create(context.TODO(), serviceAccount); errSvc != nil {
			if errors.IsAlreadyExists(errSvc) {
				log.Info("Pod service account already exist,no need to create")
				return nil
			}
			log.Error(errSvc, "Pod Service Account create error!", "Pod.ServiceAccount.Error", errSvc)
			return errSvc
		}
		log.Info("Pod ServiceAccount created successfully", "service account name", serviceAccount.Name)
		r.Recorder.Eventf(instance, v1.EventTypeNormal, "Created", "Created service account %s", serviceAccount.Name)
		return nil
	}

	return nil
}

func (r *RayClusterReconciler) reconcileAutoscalerRole(instance *rayiov1alpha1.RayCluster) error {
	if instance.Spec.EnableInTreeAutoscaling == nil || !*instance.Spec.EnableInTreeAutoscaling {
		return nil
	}

	role := &rbacv1.Role{}
	namespacedName := types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}
	if err := r.Get(context.TODO(), namespacedName, role); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}

		// Create role for autoscaler if there's no existing one in the cluster.
		role, err := common.BuildRole(instance)
		if err != nil {
			return err
		}

		// making sure the name is valid
		role.Name = utils.CheckName(role.Name)
		// Set controller reference
		if err := controllerutil.SetControllerReference(instance, role, r.Scheme); err != nil {
			return err
		}

		if errSvc := r.Create(context.TODO(), role); errSvc != nil {
			if errors.IsAlreadyExists(errSvc) {
				log.Info("role already exist,no need to create")
				return nil
			}
			log.Error(errSvc, "Role create error!", "Role.Error", errSvc)
			return errSvc
		}
		log.Info("Role created successfully", "role name", role.Name)
		r.Recorder.Eventf(instance, v1.EventTypeNormal, "Created", "Created role %s", role.Name)
		return nil
	}

	return nil
}

func (r *RayClusterReconciler) reconcileAutoscalerRoleBinding(instance *rayiov1alpha1.RayCluster) error {
	if instance.Spec.EnableInTreeAutoscaling == nil || !*instance.Spec.EnableInTreeAutoscaling {
		return nil
	}

	roleBinding := &rbacv1.RoleBinding{}
	namespacedName := types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}
	if err := r.Get(context.TODO(), namespacedName, roleBinding); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}

		// Create role bindings for autoscaler if there's no existing one in the cluster.
		roleBinding, err := common.BuildRoleBinding(instance)
		if err != nil {
			return err
		}

		// making sure the name is valid
		roleBinding.Name = utils.CheckName(roleBinding.Name)
		// Set controller reference
		if err := controllerutil.SetControllerReference(instance, roleBinding, r.Scheme); err != nil {
			return err
		}

		if errSvc := r.Create(context.TODO(), roleBinding); errSvc != nil {
			if errors.IsAlreadyExists(errSvc) {
				log.Info("role binding already exist,no need to create")
				return nil
			}
			log.Error(errSvc, "Role binding create error!", "RoleBinding.Error", errSvc)
			return errSvc
		}
		log.Info("RoleBinding created successfully", "role binding name", roleBinding.Name)
		r.Recorder.Eventf(instance, v1.EventTypeNormal, "Created", "Created role binding %s", roleBinding.Name)
		return nil
	}

	return nil
}
