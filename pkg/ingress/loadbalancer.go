package ingress

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/appscode/go/errors"
	"github.com/appscode/go/log"
	"github.com/appscode/go/types"
	api "github.com/appscode/voyager/apis/voyager/v1beta1"
	acs "github.com/appscode/voyager/client/typed/voyager/v1beta1"
	"github.com/appscode/voyager/pkg/config"
	"github.com/appscode/voyager/pkg/eventer"
	"github.com/appscode/voyager/pkg/monitor"
	_ "github.com/appscode/voyager/third_party/forked/cloudprovider/providers"
	pcm "github.com/coreos/prometheus-operator/pkg/client/monitoring/v1alpha1"
	apps "k8s.io/api/apps/v1beta1"
	apiv1 "k8s.io/api/core/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	core "k8s.io/client-go/listers/core/v1"
)

type loadBalancerController struct {
	*controller
}

var _ Controller = &loadBalancerController{}

func NewLoadBalancerController(
	ctx context.Context,
	kubeClient clientset.Interface,
	crdClient apiextensionsclient.Interface,
	extClient acs.VoyagerV1beta1Interface,
	promClient pcm.MonitoringV1alpha1Interface,
	serviceLister core.ServiceLister,
	endpointsLister core.EndpointsLister,
	opt config.Options,
	ingress *api.Ingress) Controller {
	return &loadBalancerController{
		controller: &controller{
			logger:          log.New(ctx),
			KubeClient:      kubeClient,
			CRDClient:       crdClient,
			VoyagerClient:   extClient,
			PromClient:      promClient,
			ServiceLister:   serviceLister,
			EndpointsLister: endpointsLister,
			Opt:             opt,
			Ingress:         ingress,
			recorder:        eventer.NewEventRecorder(kubeClient, "voyager operator"),
		},
	}
}

func (c *loadBalancerController) IsExists() bool {
	_, err := c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		return false
	}
	_, err = c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		return false
	}
	_, err = c.KubeClient.CoreV1().ConfigMaps(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		return false
	}
	if c.Opt.EnableRBAC {
		_, err = c.KubeClient.CoreV1().ServiceAccounts(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
		if kerr.IsNotFound(err) {
			return false
		}
		_, err = c.KubeClient.RbacV1beta1().Roles(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
		if kerr.IsNotFound(err) {
			return false
		}
		_, err = c.KubeClient.RbacV1beta1().RoleBindings(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
		if kerr.IsNotFound(err) {
			return false
		}
	}
	return true
}

func (c *loadBalancerController) Create() error {
	err := c.generateConfig()
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeWarning,
			eventer.EventReasonIngressHAProxyConfigCreateFailed,
			"Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	err = c.ensureConfigMap()
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeWarning,
			eventer.EventReasonIngressConfigMapCreateFailed,
			"Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		apiv1.EventTypeNormal,
		eventer.EventReasonIngressConfigMapCreateSuccessful,
		"Successfully created ConfigMap %s",
		c.Ingress.OffshootName(),
	)

	// If RBAC is enabled we need to ensure service account
	if c.Opt.EnableRBAC {
		err := c.ensureRBAC()
		if err != nil {
			c.recorder.Event(
				c.Ingress.ObjectReference(),
				apiv1.EventTypeWarning,
				eventer.EventReasonIngressRBACFailed,
				err.Error(),
			)
			return errors.FromErr(err).Err()
		}
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeNormal,
			eventer.EventReasonIngressRBACSuccessful,
			"Successfully applied RBAC",
		)
	}

	// deleteResidualPods is a safety checking deletion of previous version RC
	// This should Ignore error.
	c.deleteResidualPods()
	_, err = c.ensurePods(nil)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeWarning,
			eventer.EventReasonIngressControllerCreateFailed,
			"Failed to create NodePortPods, Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		apiv1.EventTypeNormal,
		eventer.EventReasonIngressControllerCreateSuccessful,
		"Successfully created NodePortPods",
	)

	_, err = c.ensureService(nil)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeWarning,
			eventer.EventReasonIngressServiceCreateFailed,
			"Failed to create LoadBalancerService, Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		apiv1.EventTypeNormal,
		eventer.EventReasonIngressServiceCreateSuccessful,
		"Successfully created LoadBalancerService",
	)

	go c.updateStatus()

	if c.Ingress.Stats() {
		err := c.ensureStatsService()
		// Error ignored intentionally
		if err != nil {
			c.recorder.Eventf(
				c.Ingress.ObjectReference(),
				apiv1.EventTypeWarning,
				eventer.EventReasonIngressStatsServiceCreateFailed,
				"Failed to create Stats Service. Reason: %s",
				err.Error(),
			)
		} else {
			c.recorder.Eventf(
				c.Ingress.ObjectReference(),
				apiv1.EventTypeNormal,
				eventer.EventReasonIngressStatsServiceCreateSuccessful,
				"Successfully created Stats Service %s",
				c.Ingress.StatsServiceName(),
			)
		}
	}

	monSpec, err := c.Ingress.MonitorSpec()
	if err != nil {
		return errors.FromErr(err).Err()
	}
	if monSpec != nil && monSpec.Prometheus != nil {
		ctrl := monitor.NewPrometheusController(c.KubeClient, c.CRDClient, c.PromClient)
		err := ctrl.AddMonitor(c.Ingress, monSpec)
		// Error Ignored intentionally
		if err != nil {
			c.recorder.Eventf(
				c.Ingress.ObjectReference(),
				apiv1.EventTypeWarning,
				eventer.EventReasonIngressServiceMonitorCreateFailed,
				err.Error(),
			)
		} else {
			c.recorder.Eventf(
				c.Ingress.ObjectReference(),
				apiv1.EventTypeNormal,
				eventer.EventReasonIngressServiceMonitorCreateSuccessful,
				"Successfully created ServiceMonitor",
			)
		}
	}

	return nil
}

func (c *loadBalancerController) Update(mode UpdateMode, old *api.Ingress) error {
	err := c.generateConfig()
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeWarning,
			eventer.EventReasonIngressHAProxyConfigCreateFailed,
			"Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	// Update HAProxy config
	err = c.updateConfigMap()
	if err != nil {
		return errors.FromErr(err).Err()
	}

	// If RBAC is enabled we need to ensure service account
	if c.Opt.EnableRBAC {
		err := c.ensureRBAC()
		if err != nil {
			c.recorder.Event(
				c.Ingress.ObjectReference(),
				apiv1.EventTypeWarning,
				eventer.EventReasonIngressRBACFailed,
				err.Error(),
			)
			return errors.FromErr(err).Err()
		}
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeNormal,
			eventer.EventReasonIngressRBACSuccessful,
			"Successfully applied RBAC",
		)
	}

	_, err = c.ensurePods(old)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeWarning,
			eventer.EventReasonIngressUpdateFailed,
			"Failed to update Pods, %s", err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		apiv1.EventTypeNormal,
		eventer.EventReasonIngressUpdateSuccessful,
		"Successfully updated Pods",
	)

	_, err = c.ensureService(old)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			apiv1.EventTypeWarning,
			eventer.EventReasonIngressServiceUpdateFailed,
			"Failed to update LBService, %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		apiv1.EventTypeNormal,
		eventer.EventReasonIngressServiceUpdateSuccessful,
		"Successfully updated LBService",
	)

	go c.updateStatus()

	if mode&UpdateStats > 0 {
		if c.Ingress.Stats() {
			err := c.ensureStatsService()
			if err != nil {
				c.recorder.Eventf(
					c.Ingress.ObjectReference(),
					apiv1.EventTypeWarning,
					eventer.EventReasonIngressStatsServiceCreateFailed,
					"Failed to create HAProxy stats Service. Reason: %s",
					err.Error(),
				)
			} else {
				c.recorder.Eventf(
					c.Ingress.ObjectReference(),
					apiv1.EventTypeNormal,
					eventer.EventReasonIngressStatsServiceCreateSuccessful,
					"Successfully created HAProxy stats Service %s",
					c.Ingress.StatsServiceName(),
				)
			}
		} else {
			err := c.ensureStatsServiceDeleted()
			if err != nil {
				c.recorder.Eventf(
					c.Ingress.ObjectReference(),
					apiv1.EventTypeWarning,
					eventer.EventReasonIngressStatsServiceDeleteFailed,
					"Failed to delete HAProxy stats Service. Reason: %s",
					err.Error(),
				)
			} else {
				c.recorder.Eventf(
					c.Ingress.ObjectReference(),
					apiv1.EventTypeNormal,
					eventer.EventReasonIngressStatsServiceDeleteSuccessful,
					"Successfully deleted HAProxy stats Service %s",
					c.Ingress.StatsServiceName(),
				)
			}
		}
	}
	return nil
}

func (c *loadBalancerController) EnsureFirewall(svc *apiv1.Service) error {
	return nil
}

func (c *loadBalancerController) Delete() {
	// Ignore Error.
	c.deleteResidualPods()
	err := c.deletePods()
	if err != nil {
		c.logger.Errorln(err)
	}
	err = c.deleteConfigMap()
	if err != nil {
		c.logger.Errorln(err)
	}
	if c.Opt.EnableRBAC {
		if err := c.ensureRBACDeleted(); err != nil {
			c.logger.Errorln(err)
		}
	}
	err = c.ensureServiceDeleted()
	if err != nil {
		c.logger.Errorln(err)
	}
	monSpec, err := c.Ingress.MonitorSpec()
	if err != nil {
		c.logger.Errorln(err)
	}
	if monSpec != nil && monSpec.Prometheus != nil {
		ctrl := monitor.NewPrometheusController(c.KubeClient, c.CRDClient, c.PromClient)
		ctrl.DeleteMonitor(c.Ingress, monSpec)
	}
	if c.Ingress.Stats() {
		c.ensureStatsServiceDeleted()
	}
	return
}

func (c *loadBalancerController) newService() *apiv1.Service {
	svc := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Ingress.OffshootName(),
			Namespace: c.Ingress.Namespace,
			Annotations: map[string]string{
				api.OriginAPISchema: c.Ingress.APISchema(),
				api.OriginName:      c.Ingress.GetName(),
			},
		},
		Spec: apiv1.ServiceSpec{
			Type:                     apiv1.ServiceTypeLoadBalancer,
			Ports:                    []apiv1.ServicePort{},
			Selector:                 c.Ingress.OffshootLabels(),
			LoadBalancerSourceRanges: c.Ingress.Spec.LoadBalancerSourceRanges,
		},
	}
	svc.ObjectMeta = c.ensureOwnerReference(svc.ObjectMeta)

	// opening other tcp ports
	mappings, _ := c.Ingress.PortMappings(c.Opt.CloudProvider)
	for svcPort, target := range mappings {
		p := apiv1.ServicePort{
			Name:       "tcp-" + strconv.Itoa(svcPort),
			Protocol:   "TCP",
			Port:       int32(svcPort),
			TargetPort: intstr.FromInt(target.PodPort),
			NodePort:   int32(target.NodePort),
		}
		svc.Spec.Ports = append(svc.Spec.Ports, p)
	}

	if ans, ok := c.Ingress.ServiceAnnotations(c.Opt.CloudProvider); ok {
		for k, v := range ans {
			svc.Annotations[k] = v
		}
	}

	if c.Ingress.LBType() == api.LBTypeLoadBalancer && c.Ingress.KeepSourceIP() {
		switch c.Opt.CloudProvider {
		case "gce", "gke", "azure", "acs":
			// https://github.com/appscode/voyager/issues/276
			// ref: https://kubernetes.io/docs/tutorials/services/source-ip/#source-ip-for-services-with-typeloadbalancer
			svc.Spec.ExternalTrafficPolicy = apiv1.ServiceExternalTrafficPolicyTypeLocal
		}
	}

	switch c.Opt.CloudProvider {
	case "gce", "gke", "azure", "acs":
		if ip := c.Ingress.LoadBalancerIP(); ip != nil {
			svc.Spec.LoadBalancerIP = ip.String()
		}
	}
	return svc
}

func (c *loadBalancerController) ensureService(old *api.Ingress) (*apiv1.Service, error) {
	desired := c.newService()
	current, err := c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Get(desired.Name, metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		c.logger.Infof("Creating Service %s/%s", desired.Namespace, desired.Name)
		return c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Create(desired)
	} else if err != nil {
		return nil, err
	}
	if svc, needsUpdate := c.serviceRequiresUpdate(current, desired, old); needsUpdate {
		c.logger.Infof("Updating Service %s/%s", desired.Namespace, desired.Name)
		return c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Update(svc)
	}
	return current, nil
}

func (c *loadBalancerController) newPods() *apps.Deployment {
	deployment := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Ingress.OffshootName(),
			Namespace: c.Ingress.Namespace,
			Labels:    c.Ingress.OffshootLabels(),
			Annotations: map[string]string{
				api.OriginAPISchema: c.Ingress.APISchema(),
				api.OriginName:      c.Ingress.GetName(),
			},
		},

		Spec: apps.DeploymentSpec{
			Replicas: types.Int32P(c.Ingress.Replicas()),
			Selector: &metav1.LabelSelector{
				MatchLabels: c.Ingress.OffshootLabels(),
			},
			// pod templates.
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: c.Ingress.OffshootLabels(),
				},
				Spec: apiv1.PodSpec{
					Affinity:      c.Ingress.Spec.Affinity,
					SchedulerName: c.Ingress.Spec.SchedulerName,
					Tolerations:   c.Ingress.Spec.Tolerations,
					NodeSelector:  c.Ingress.NodeSelector(),
					Containers: []apiv1.Container{
						{
							Name:  "haproxy",
							Image: c.Opt.HAProxyImage,
							Args: []string{
								"--ingress-api-version=" + c.Ingress.APISchema(),
								"--ingress-name=" + c.Ingress.Name,
								"--cloud-provider=" + c.Opt.CloudProvider,
								"--v=3",
								fmt.Sprintf("--qps=%v", c.Opt.QPS),
								fmt.Sprintf("--burst=%v", c.Opt.Burst),
								"--boot-cmd=" + "/etc/sv/haproxy/reload",
								"--configmap=" + c.Ingress.OffshootName(),
								"--mount-location=" + "/etc/haproxy",
							},
							Ports:     []apiv1.ContainerPort{},
							Resources: c.Ingress.Spec.Resources,
							VolumeMounts: []apiv1.VolumeMount{
								{
									Name:      TLSCertificateVolumeName,
									MountPath: "/etc/ssl/private/haproxy",
								},
							},
						},
					},
					Volumes: []apiv1.Volume{
						{
							Name: TLSCertificateVolumeName,
							VolumeSource: apiv1.VolumeSource{
								EmptyDir: &apiv1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
	deployment.ObjectMeta = c.ensureOwnerReference(deployment.ObjectMeta)
	deployment.Spec.Template.Spec.Containers[0].Env = c.ensureEnvVars(deployment.Spec.Template.Spec.Containers[0].Env)

	if c.Opt.EnableRBAC {
		deployment.Spec.Template.Spec.ServiceAccountName = c.Ingress.OffshootName()
	}

	exporter, _ := c.getExporterSidecar()
	if exporter != nil {
		deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers, *exporter)
	}

	// adding tcp ports to pod template
	for _, podPort := range c.Ingress.PodPorts() {
		p := apiv1.ContainerPort{
			Name:          "tcp-" + strconv.Itoa(podPort),
			Protocol:      "TCP",
			ContainerPort: int32(podPort),
		}
		deployment.Spec.Template.Spec.Containers[0].Ports = append(deployment.Spec.Template.Spec.Containers[0].Ports, p)
	}

	if c.Ingress.Stats() {
		deployment.Spec.Template.Spec.Containers[0].Ports = append(deployment.Spec.Template.Spec.Containers[0].Ports, apiv1.ContainerPort{
			Name:          api.StatsPortName,
			Protocol:      "TCP",
			ContainerPort: int32(c.Ingress.StatsPort()),
		})
	}

	if ans, ok := c.Ingress.PodsAnnotations(); ok {
		deployment.Spec.Template.Annotations = ans
	}

	if len(c.Ingress.ErrorFilesConfigMapName()) > 0 {
		deployment.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deployment.Spec.Template.Spec.Containers[0].VolumeMounts,
			apiv1.VolumeMount{
				Name:      ErrorFilesVolumeName,
				MountPath: ErrorFilesLocation,
			})

		deployment.Spec.Template.Spec.Volumes = append(
			deployment.Spec.Template.Spec.Volumes,
			apiv1.Volume{
				Name: ErrorFilesVolumeName,
				VolumeSource: apiv1.VolumeSource{
					ConfigMap: &apiv1.ConfigMapVolumeSource{
						LocalObjectReference: apiv1.LocalObjectReference{
							Name: c.Ingress.ErrorFilesConfigMapName(),
						},
					},
				},
			})
	}
	return deployment
}

func (c *loadBalancerController) ensurePods(old *api.Ingress) (*apps.Deployment, error) {
	desired := c.newPods()
	current, err := c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Get(desired.Name, metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		c.logger.Infof("Creating Deployment %s/%s", desired.Namespace, desired.Name)
		return c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Create(desired)
	} else if err != nil {
		return nil, err
	}

	needsUpdate := false

	// annotations
	if current.Annotations == nil {
		current.Annotations = make(map[string]string)
	}
	oldAnn := map[string]string{}
	if old != nil {
		if a, ok := old.PodsAnnotations(); ok {
			oldAnn = a
		}
	}
	for k, v := range desired.Annotations {
		if cv, found := current.Annotations[k]; !found || cv != v {
			current.Annotations[k] = v
			needsUpdate = true
		}
		delete(oldAnn, k)
	}
	for k := range oldAnn {
		if _, ok := current.Annotations[k]; ok {
			delete(current.Annotations, k)
			needsUpdate = true
		}
	}

	if !reflect.DeepEqual(current.Spec.Selector, desired.Spec.Selector) {
		needsUpdate = true
		current.Spec.Selector = desired.Spec.Selector
	}
	if !reflect.DeepEqual(current.Spec.Template.ObjectMeta, desired.Spec.Template.ObjectMeta) {
		needsUpdate = true
		current.Spec.Template.ObjectMeta = desired.Spec.Template.ObjectMeta
	}
	if !reflect.DeepEqual(current.Spec.Template.Annotations, desired.Spec.Template.Annotations) {
		needsUpdate = true
		current.Spec.Template.Annotations = desired.Spec.Template.Annotations
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.Affinity, desired.Spec.Template.Spec.Affinity) {
		needsUpdate = true
		current.Spec.Template.Spec.Affinity = desired.Spec.Template.Spec.Affinity
	}
	if current.Spec.Template.Spec.SchedulerName != desired.Spec.Template.Spec.SchedulerName {
		needsUpdate = true
		current.Spec.Template.Spec.SchedulerName = desired.Spec.Template.Spec.SchedulerName
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.Tolerations, desired.Spec.Template.Spec.Tolerations) {
		needsUpdate = true
		current.Spec.Template.Spec.Tolerations = desired.Spec.Template.Spec.Tolerations
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.NodeSelector, desired.Spec.Template.Spec.NodeSelector) {
		needsUpdate = true
		current.Spec.Template.Spec.NodeSelector = desired.Spec.Template.Spec.NodeSelector
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.Containers, desired.Spec.Template.Spec.Containers) {
		needsUpdate = true
		current.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.Volumes, desired.Spec.Template.Spec.Volumes) {
		needsUpdate = true
		current.Spec.Template.Spec.Volumes = desired.Spec.Template.Spec.Volumes
	}
	if current.Spec.Template.Spec.ServiceAccountName != desired.Spec.Template.Spec.ServiceAccountName {
		needsUpdate = true
		current.Spec.Template.Spec.ServiceAccountName = desired.Spec.Template.Spec.ServiceAccountName
	}
	if needsUpdate {
		c.logger.Infof("Updating Deployment %s/%s", desired.Namespace, desired.Name)
		return c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Update(current)
	}
	return current, nil
}

func (c *loadBalancerController) deletePods() error {
	policy := metav1.DeletePropagationForeground
	err := c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Delete(c.Ingress.OffshootName(), &metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil {
		return err
	}
	return c.deletePodsForSelector(&metav1.LabelSelector{MatchLabels: c.Ingress.OffshootLabels()})
}

// Deprecated, creating pods using RC is now deprecated.
func (c *loadBalancerController) deleteResidualPods() error {
	rc, err := c.KubeClient.CoreV1().ReplicationControllers(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
	if err != nil {
		c.logger.Warningln(err)
		return err
	}
	// resize the controller to zero (effectively deleting all pods) before deleting it.
	rc.Spec.Replicas = types.Int32P(0)
	_, err = c.KubeClient.CoreV1().ReplicationControllers(c.Ingress.Namespace).Update(rc)
	if err != nil {
		c.logger.Warningln(err)
		return err
	}

	c.logger.Debugln("Waiting before delete the RC")
	time.Sleep(time.Second * 5)
	// if update failed still trying to delete the controller.
	falseVar := false
	err = c.KubeClient.CoreV1().ReplicationControllers(c.Ingress.Namespace).Delete(c.Ingress.OffshootName(), &metav1.DeleteOptions{
		OrphanDependents: &falseVar,
	})
	if err != nil {
		c.logger.Warningln(err)
		return err
	}

	return c.deletePodsForSelector(&metav1.LabelSelector{MatchLabels: rc.Spec.Selector})
}

func (c *loadBalancerController) updateStatus() error {
	var statuses []apiv1.LoadBalancerIngress

	for i := 0; i < 50; i++ {
		time.Sleep(time.Second * 10)
		if svc, err := c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{}); err == nil {
			if len(svc.Status.LoadBalancer.Ingress) >= 1 {
				statuses = svc.Status.LoadBalancer.Ingress
				break
			}
		}
	}

	if len(statuses) > 0 {
		if c.Ingress.APISchema() == api.APISchemaIngress {
			ing, err := c.KubeClient.ExtensionsV1beta1().Ingresses(c.Ingress.Namespace).Get(c.Ingress.Name, metav1.GetOptions{})
			if err != nil {
				return errors.FromErr(err).Err()
			}
			ing.Status.LoadBalancer.Ingress = statuses
			_, err = c.KubeClient.ExtensionsV1beta1().Ingresses(c.Ingress.Namespace).Update(ing)
			if err != nil {
				return errors.FromErr(err).Err()
			}
		} else {
			ing, err := c.VoyagerClient.Ingresses(c.Ingress.Namespace).Get(c.Ingress.Name, metav1.GetOptions{})
			if err != nil {
				return errors.FromErr(err).Err()
			}
			ing.Status.LoadBalancer.Ingress = statuses
			_, err = c.VoyagerClient.Ingresses(c.Ingress.Namespace).Update(ing)
			if err != nil {
				return errors.FromErr(err).Err()
			}
		}
	}
	return nil
}
