/* SPDX-License-Identifier: Apache-2.0 */
/* Copyright(c) 2019 Wind River Systems, Inc. */

package manager

import (
	"context"
	"github.com/gophercloud/gophercloud"
	perrors "github.com/pkg/errors"
	"github.com/wind-river/cloud-platform-deployment-manager/pkg/apis/starlingx/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"strconv"
	"sync"
)

var log = logf.Log.WithName("manager")

const HTTPSNotEnabled = "server gave HTTP response to HTTPS client"

const (
	// Defines HTTP and HTTPS URL prefixes.
	HTTPSPrefix = "https://"
	HTTPPrefix  = "http://"
)

// Well-known name of the secret which holds the system API endpoint attributes (e.g., OS_USERNAME, OS_*)
const SystemEndpointSecretName = "system-endpoint"

const (
	// Defines annotation keys for resources.
	NotificationCountKey = "deployment-manager/notifications"
	ReconcileAfterInSync = "deployment-manager/reconcile-after-insync"
)

// CloudManager wraps a runtime manager and provides the ability to
// coordinate certain function across different controllers.
type CloudManager interface {
	ResetPlatformClient(namespace string) error
	GetPlatformClient(namespace string) *gophercloud.ServiceClient
	GetKubernetesClient() client.Client
	BuildPlatformClient(namespace string) (*gophercloud.ServiceClient, error)
	NotifySystemDependencies(namespace string) error
	NotifyResource(object runtime.Object) error
	SetSystemReady(namespace string, value bool)
	GetSystemReady(namespace string) bool
	SetSystemType(namespace string, value SystemType)
	GetSystemType(namespace string) SystemType
	StartMonitor(monitor *Monitor, message string) error
	CancelMonitor(object runtime.Object)
}

type SystemType string

// Defines the current list of system types.
const (
	SystemTypeAllInOne SystemType = "all-in-one"
	SystemTypeStandard SystemType = "standard"
)

type SystemMode string

// Defines the current list of system modes
const (
	SystemModeSimplex SystemMode = "simplex"
	SystemModeDuplex  SystemMode = "duplex"
)

type SystemNamespace struct {
	client     *gophercloud.ServiceClient
	ready      bool
	systemType SystemType
}

type PlatformManager struct {
	manager.Manager
	lock     sync.Mutex
	systems  map[string]*SystemNamespace
	monitors map[string]*Monitor
}

func NewPlatformManager(manager manager.Manager) CloudManager {
	return &PlatformManager{
		Manager:  manager,
		systems:  make(map[string]*SystemNamespace),
		monitors: make(map[string]*Monitor),
	}
}

// BaseError defines a common Error implementation for all manager errors
// that derive from this structure.
type BaseError struct {
	message string
}

// Error implements the Error interface for all structures that derive from
// BaseError.
func (in BaseError) Error() string {
	return in.message
}

// ClientError defines an error to be used on an semantic error encountered
// while attempting to build a platform client object.
type ClientError struct {
	BaseError
}

// NewClientError defines a wrapper to correctly instantiate a manager client
// error.
func NewClientError(msg string) error {
	return perrors.WithStack(ClientError{BaseError{msg}})
}

// WaitForMonitor defines a special error object that signals to the
// common error handler that a monitor has been launched to trigger another
// reconciliation attempt only when certain criteria have been met.
type WaitForMonitor struct {
	BaseError
}

// NewWaitForMonitor defines a constructor for the WaitForMonitor error type.
func NewWaitForMonitor(msg string) error {
	return WaitForMonitor{BaseError{msg}}
}

// getNextCount takes a number in string form and returns the next sequential
// value.  If the initial value is not a number then 0 as used as the initial
// input value.
func getNextCount(value string) string {
	var err error

	count := 0
	if value != "" {
		count, err = strconv.Atoi(value)
		if err != nil {
			log.Info("unexpected annotation", "value", value)
			count = 0
		}
	}

	return strconv.Itoa(count + 1)
}

func (m *PlatformManager) NotifySystemController(namespace string) error {
	systems := &v1.SystemList{}
	opts := client.ListOptions{}
	opts.InNamespace(namespace)
	err := m.GetClient().List(context.TODO(), &opts, systems)
	if err != nil {
		err = perrors.Wrap(err, "failed to query system list")
		return err
	}

	// There should only be a single system, but for the sake of completeness
	// update any instance returned by the API.
	for _, obj := range systems.Items {
		count := getNextCount(obj.Annotations[NotificationCountKey])
		obj.Annotations[NotificationCountKey] = count

		err := m.GetClient().Update(context.TODO(), &obj)
		if err != nil {
			err = perrors.Wrap(err, "failed to notify system controller")
			return err
		}

		log.Info("system controller has been notified", "name", obj.Name)
	}

	return nil
}

// systemDependencies defines the list of controllers to be notified on a
// system event.  Only those controllers that are managing external resources
// need to be notified.  HostProfiles are consumed by Host resources therefore
// do not need to be notified.
var systemDependencies = []schema.GroupVersionKind{
	{Group: v1.Group,
		Version: v1.Version,
		Kind:    v1.KindHost},
	{Group: v1.Group,
		Version: v1.Version,
		Kind:    v1.KindPlatformNetwork},
	{Group: v1.Group,
		Version: v1.Version,
		Kind:    v1.KindDataNetwork},
	{Group: v1.Group,
		Version: v1.Version,
		Kind:    v1.KindPTPInstance},
	{Group: v1.Group,
		Version: v1.Version,
		Kind:    v1.KindPTPInterface},
}

// notifyControllers updates an annotation on each of the listed controller
// kinds to force each to re-run its reconcile loop.  This should only be
// executed by the system controller.
func (m *PlatformManager) notifyControllers(namespace string, gvkList []schema.GroupVersionKind) error {
	for _, gvk := range gvkList {
		objects := &unstructured.UnstructuredList{}
		objects.SetGroupVersionKind(gvk)
		opts := client.ListOptions{}
		opts.InNamespace(namespace)
		err := m.GetClient().List(context.TODO(), &opts, objects)
		if err != nil {
			err = perrors.Wrapf(err, "failed to query %s list", gvk.Kind)
			return err
		}

		for _, obj := range objects.Items {
			switch obj.GetKind() {
			case v1.KindHost, v1.KindHostProfile, v1.KindPlatformNetwork, v1.KindDataNetwork, v1.KindPTPInstance, v1.KindPTPInterface:
				annotations := obj.GetAnnotations()
				if annotations == nil {
					annotations = make(map[string]string)
				}

				count := getNextCount(annotations[NotificationCountKey])
				annotations[NotificationCountKey] = count

				obj.SetAnnotations(annotations)

				err := m.GetClient().Update(context.TODO(), &obj)
				if err != nil {
					err = perrors.Wrapf(err, "failed to notify %s controller", obj.GetKind())
					return err
				}

				log.Info("controller has been notified", "name", obj.GetName(), "kind", obj.GetKind())
			}
		}
	}

	return nil
}

// notifyController updates an annotation on a single controller to force it
// to re-run its reconcile loop.
func (m *PlatformManager) notifyController(object runtime.Object) error {
	key, err := client.ObjectKeyFromObject(object)
	if err != nil {
		return err
	}

	result := object.DeepCopyObject()
	err = m.GetClient().Get(context.Background(), key, result)
	if err != nil {
		err = perrors.Wrapf(err, "failed to query resource %+v", key)
		return err
	}

	accessor := meta.NewAccessor()

	annotations, err := accessor.Annotations(result)
	if err != nil {
		err = perrors.Wrap(err, "failed to get annotations via accessor")
		return err
	}

	if annotations == nil {
		annotations = make(map[string]string)
	}

	count := getNextCount(annotations[NotificationCountKey])
	annotations[NotificationCountKey] = count

	err = accessor.SetAnnotations(result, annotations)
	if err != nil {
		err = perrors.Wrap(err, "failed to set annotations via accessor")
		return err
	}

	err = m.GetClient().Update(context.TODO(), result)
	if err != nil {
		err = perrors.Wrapf(err, "failed to notify host controller")
		return err
	}

	log.V(2).Info("controller has been notified", "key", key)

	return nil
}

func (m *PlatformManager) NotifySystemDependencies(namespace string) error {
	return m.notifyControllers(namespace, systemDependencies)
}

func (m *PlatformManager) NotifyResource(object runtime.Object) error {
	return m.notifyController(object)
}

// GetKubernetesClient returns a reference to the Kubernetes client
func (m *PlatformManager) GetKubernetesClient() client.Client {
	return m.GetClient()
}

// GetPlatformClient returns the instance of the platform manager for a given
// namespace.  It is has not been created yet then false is returned in the
// second return parameter.
func (m *PlatformManager) GetPlatformClient(namespace string) *gophercloud.ServiceClient {
	m.lock.Lock()
	defer func() { m.lock.Unlock() }()

	// Look for an existing client
	if obj, ok := m.systems[namespace]; ok {
		return obj.client
	}

	return nil
}

// ResetPlatformClient deletes the instance of the platform manager for a
// given namespace.
func (m *PlatformManager) ResetPlatformClient(namespace string) error {
	m.lock.Lock()
	defer func() { m.lock.Unlock() }()

	// Look for an existing client
	if obj, ok := m.systems[namespace]; ok {
		if obj.client == nil {
			// Already reset.
			return nil
		}
		obj.client = nil
	} else {
		// SystemNamespace doesn't exist yet
		return nil
	}

	// The system controller is the master of the client so notify it so that
	// it can recreate it.
	return m.NotifySystemController(namespace)
}

// SetSystemReady allows setting the readiness state for a given namespace.
func (m *PlatformManager) SetSystemReady(namespace string, value bool) {
	m.lock.Lock()
	defer func() { m.lock.Unlock() }()

	if obj, ok := m.systems[namespace]; !ok {
		m.systems[namespace] = &SystemNamespace{ready: value}
	} else {
		obj.ready = value
	}
}

// GetSystemReady returns whether the system for the specified namespace
// is ready for all controllers to reconcile their resources.
func (m *PlatformManager) GetSystemReady(namespace string) bool {
	m.lock.Lock()
	defer func() { m.lock.Unlock() }()

	if obj, ok := m.systems[namespace]; !ok {
		return false
	} else {
		return obj.ready
	}
}

// SetSystemReady allows setting the readiness state for a given namespace.
func (m *PlatformManager) SetSystemType(namespace string, value SystemType) {
	m.lock.Lock()
	defer func() { m.lock.Unlock() }()

	if obj, ok := m.systems[namespace]; !ok {
		m.systems[namespace] = &SystemNamespace{systemType: value}
		log.Info("system type has been set", "type", value)
	} else if obj.systemType != value {
		obj.systemType = value
		log.Info("system type has been updated", "type", value)
	}

}

// GetSystemReady returns whether the system for the specified namespace
// is ready for all controllers to reconcile their resources.
func (m *PlatformManager) GetSystemType(namespace string) SystemType {
	m.lock.Lock()
	defer func() { m.lock.Unlock() }()

	if obj, ok := m.systems[namespace]; !ok {
		return ""
	} else {
		return obj.systemType
	}
}

// StartMonitor starts the specified monitor, generates an event, and then
// return an error suitable to stop the reconciler from running until the
// monitor has explicitly triggered a new reconcilable event.
func (m *PlatformManager) StartMonitor(monitor *Monitor, message string) error {
	m.lock.Lock()
	defer func() { m.lock.Unlock() }()

	key := monitor.GetKey()
	m.monitors[key] = monitor

	log.V(2).Info("starting monitor", "key", key)

	// Run the monitor.
	monitor.Start(m)

	// Return an error which has specific handling to stop and wait for the
	// monitor
	return NewWaitForMonitor(message)
}

// CancelMonitor stops any monitor currently running against the resource
// being reconciled.
func (m *PlatformManager) CancelMonitor(object runtime.Object) {
	m.lock.Lock()
	defer func() { m.lock.Unlock() }()

	key := BuildMonitorKey(object)
	if monitor, ok := m.monitors[key]; ok {
		log.V(2).Info("stopping monitor", "key", key)
		monitor.Stop()
		delete(m.monitors, key)
	}
}

var instance CloudManager
var once sync.Once

// GetInstance returns a singleton instance of the platform manager
func GetInstance(mgr manager.Manager) CloudManager {
	once.Do(func() {
		instance = NewPlatformManager(mgr)
	})

	return instance
}
