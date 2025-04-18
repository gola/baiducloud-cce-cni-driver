package bcesync

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"

	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/api/v1/models"
	operatorOption "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/operator/option"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/bce/api/cloud"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/bce/api/eni"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/bce/option"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s"
	ccev2 "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/apis/cce.baidubce.com/v2"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/watchers/cm"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/logging"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/logging/logfields"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/syncer"
	enisdk "github.com/baidubce/bce-sdk-go/services/eni"
)

const (
	eniControllerName = "eni-sync-manager"

	ENIReadyTimeToAttach = 1 * time.Second
	ENIMaxCreateDuration = 5 * time.Minute

	FinalizerENI = "eni-syncer"
)

var eniLog = logging.NewSubysLogger(eniControllerName)

// remoteEniSyncher
type remoteEniSyncher interface {
	syncENI(ctx context.Context) (result []eni.Eni, err error)
	statENI(ctx context.Context, eniID string) (*eni.Eni, error)

	// use eni machine to manager status of eni
	useENIMachine() bool

	setENIUpdater(updater syncer.ENIUpdater)
}

// VPCENISyncerRouter only work with single vpc cluster
type VPCENISyncerRouter struct {
	eni *eniSyncher
}

// NewVPCENISyncer create a new VPCENISyncer
func (es *VPCENISyncerRouter) Init(ctx context.Context) error {
	eventRecorder := k8s.EventBroadcaster().NewRecorder(scheme.Scheme, corev1.EventSource{Component: eniControllerName})
	bceclient := option.BCEClient()
	resyncPeriod := operatorOption.Config.ResourceENIResyncInterval

	// 1. init vpc remote syncer
	vpcRemote := &remoteVpcEniSyncher{
		bceclient:     bceclient,
		eventRecorder: eventRecorder,
		ClusterID:     operatorOption.Config.CCEClusterID,
	}
	es.eni = &eniSyncher{
		bceclient:    bceclient,
		resyncPeriod: resyncPeriod,

		remoteSyncer:  vpcRemote,
		eventRecorder: eventRecorder,
	}
	err := es.eni.Init(ctx)
	if err != nil {
		return fmt.Errorf("init eni syncer failed: %v", err)
	}
	vpcRemote.syncManager = es.eni.syncManager
	vpcRemote.VPCIDs = operatorOption.Config.BCECloudVPCID
	return nil
}

// StartENISyncer implements syncer.ENISyncher
func (es *VPCENISyncerRouter) StartENISyncer(ctx context.Context, updater syncer.ENIUpdater) syncer.ENIEventHandler {
	es.eni.StartENISyncer(ctx, updater)
	return es
}

// Create implements syncer.ENIEventHandler
func (es *VPCENISyncerRouter) Create(resource *ccev2.ENI) error {
	types := resource.Spec.Type
	// Remove "if types == ccev2.ENIForHPC || types == ccev2.ENIForERI return nil",
	// because we need to support the case of RDMA ENI already have RDMA IPs
	if types == ccev2.ENIForBBC {
		return nil
	}

	return es.eni.Create(resource)
}

// Delete implements syncer.ENIEventHandler
func (es *VPCENISyncerRouter) Delete(name string) error {
	return es.eni.Delete(name)
}

// ResyncENI implements syncer.ENIEventHandler
func (es *VPCENISyncerRouter) ResyncENI(ctx context.Context) time.Duration {
	return es.eni.ResyncENI(ctx)
}

// Update implements syncer.ENIEventHandler
func (es *VPCENISyncerRouter) Update(resource *ccev2.ENI) error {
	types := resource.Spec.Type
	// Remove "if types == ccev2.ENIForHPC || types == ccev2.ENIForERI return nil",
	// because we need to support the case of RDMA ENI already have RDMA IPs
	if types == ccev2.ENIForBBC {
		return nil
	}

	return es.eni.Update(resource)
}

var (
	_ syncer.ENISyncher      = &VPCENISyncerRouter{}
	_ syncer.ENIEventHandler = &VPCENISyncerRouter{}
)

// eniSyncher create SyncerManager for ENI
type eniSyncher struct {
	syncManager  *SyncManager[eni.Eni]
	updater      syncer.ENIUpdater
	bceclient    cloud.Interface
	resyncPeriod time.Duration

	remoteSyncer  remoteEniSyncher
	eventRecorder record.EventRecorder
}

// Init initialise the sync manager.
// add vpcIDs to list
func (es *eniSyncher) Init(ctx context.Context) error {
	es.syncManager = NewSyncManager(eniControllerName, es.resyncPeriod, es.remoteSyncer.syncENI)
	return nil
}

func (es *eniSyncher) StartENISyncer(ctx context.Context, updater syncer.ENIUpdater) syncer.ENIEventHandler {
	es.remoteSyncer.setENIUpdater(updater)
	es.updater = updater
	es.syncManager.Run()
	log.WithField(taskLogField, eniControllerName).Infof("ENISyncher is running")
	return es
}

// Create Process synchronization of new enis
// For a new eni, we should generally query the details of the eni directly
// and synchronously
func (es *eniSyncher) Create(resource *ccev2.ENI) error {
	log.WithField(taskLogField, eniControllerName).
		Infof("create a new eni(%s) crd", resource.Name)
	return es.Update(resource)
}

func (es *eniSyncher) Update(resource *ccev2.ENI) error {
	var err error

	scopeLog := eniLog.WithFields(logrus.Fields{
		"eniID":      resource.Name,
		"vpcID":      resource.Spec.ENI.VpcID,
		"eniName":    resource.Spec.ENI.Name,
		"instanceID": resource.Spec.ENI.InstanceID,
		"oldStatus":  resource.Status.VPCStatus,
		"method":     "eniSyncher.Update",
	})

	// refresh eni from vpc and retry if k8s resource is expired
	for retry := 0; retry < 3; retry++ {
		if retry > 0 {
			// refresh new k8s resource when resource is expired
			resource, err = k8s.CCEClient().CceV2().ENIs().Get(context.TODO(), resource.Name, metav1.GetOptions{})
			if err != nil {
				scopeLog.WithError(err).Error("get eni failed")
				return err
			}

		}
		scopeLog = scopeLog.WithField("retry", retry)
		err := es.handleENIUpdate(resource, scopeLog)
		if kerrors.IsConflict(err) || kerrors.IsResourceExpired(err) {
			continue
		}
		return err
	}

	return nil
}

func (es *eniSyncher) handleENIUpdate(resource *ccev2.ENI, scopeLog *logrus.Entry) error {
	var (
		newObj            = resource.DeepCopy()
		err               error
		ctx               = logfields.NewContext()
		eniStatus         *ccev2.ENIStatus
		updateStatusError error
	)
	defer func() {
		if err == nil {
			GlobalBSM().ForceBorrowForENI(newObj)
		}
	}()

	// delete old eni
	if newObj.Status.VPCStatus == ccev2.VPCENIStatusDeleted {
		return nil
	}

	isNeedSkipUpdate := false

	isNeedRemoveFinalizer := es.mangeFinalizer(newObj)
	if isNeedRemoveFinalizer {
		scopeLog.WithContext(ctx).WithField("eni", newObj.Name).Info("eni should remove finalizers")
		_, err = es.updater.Update(newObj)
		if err != nil {
			scopeLog.WithError(err).Errorf("patch eni %s failed", newObj.Name)
		}
		return err
	}

	if es.remoteSyncer.useENIMachine() {
		scopeLog.Debug("start eni machine")
		// start machine
		machine := eniStateMachine{
			es:       es,
			ctx:      ctx,
			resource: newObj,
			scopeLog: scopeLog,
		}

		err = machine.start()
		isNeedSkipUpdate = machine.isSync
		eniStatus = &newObj.Status
		_, isDelayError := err.(*cm.DelayEvent)
		if isDelayError {
			goto UpdateStatus
		} else if err != nil {
			scopeLog.WithError(err).Error("eni machine failed")
			return err
		}
	}

	// When the Finalizer is null, only patching is allowed, updates will ignore changes.
UpdateStatus:
	if isNeedSkipUpdate {
		return nil
	}
	obj, err := es.updater.Lister().Get(newObj.Name)
	if err != nil || obj == nil {
		scopeLog.WithError(err).Error("get eni failed")
		return err
	}
	objEniToUpdateStatus := obj.DeepCopy()
	if logfields.Json(eniStatus) != logfields.Json(&resource.Status) &&
		eniStatus != nil {
		objEniToUpdateStatus.Status = *eniStatus
		scopeLog = scopeLog.WithFields(logrus.Fields{
			"vpcStatus": objEniToUpdateStatus.Status.VPCStatus,
			"oldStatus": resource.Status.VPCStatus,
			"cceStatus": objEniToUpdateStatus.Status.CCEStatus,
		})

		_, updateStatusError = es.updater.UpdateStatus(objEniToUpdateStatus)
		if updateStatusError != nil {
			scopeLog.WithError(updateStatusError).WithField("eni", objEniToUpdateStatus.Name).Error("update eni status failed")
			return updateStatusError
		}
		scopeLog.Info("update eni status success")
	}
	return err
}

// When the Finalizer is null, only patching is allowed, updates will ignore changes.
// mangeFinalizer except for node deletion, direct deletion of ENI objects is prohibited
// Only when the ENI is not in use, the finalizer will be removed. Then, return true.
// return true: should patch this object (*ccev2.ENI)
func (*eniSyncher) mangeFinalizer(newObj *ccev2.ENI) (isNeedRemoveFinalizer bool) {
	isNeedRemoveFinalizer = false
	if newObj.DeletionTimestamp == nil && len(newObj.Finalizers) == 0 {
		newObj.Finalizers = append(newObj.Finalizers, FinalizerENI)
		isNeedRemoveFinalizer = false
	}
	var finalizers []string

	if newObj.DeletionTimestamp != nil && len(newObj.Finalizers) != 0 {
		node, err := k8s.CCEClient().Informers.Cce().V2().NetResourceSets().Lister().Get(newObj.Spec.NodeName)
		if kerrors.IsNotFound(err) {
			goto removeFinalizer
		}
		if node != nil && node.DeletionTimestamp != nil {
			goto removeFinalizer
		}
		if node != nil && len(newObj.GetOwnerReferences()) != 0 && node.GetUID() != newObj.GetOwnerReferences()[0].UID {
			goto removeFinalizer
		}

		// eni is not inuse
		if newObj.Status.VPCStatus != ccev2.VPCENIStatusDeleted &&
			newObj.Status.VPCStatus != ccev2.VPCENIStatusInuse {
			goto removeFinalizer
		}
	}
	return

removeFinalizer:
	for _, f := range newObj.Finalizers {
		if f == FinalizerENI {
			continue
		}
		finalizers = append(finalizers, f)
	}
	newObj.Finalizers = finalizers
	log.Infof("remove finalizer from deletable ENI %s on NetResourceSet %s ", newObj.Name, newObj.Spec.NodeName)
	if len(newObj.Finalizers) == 0 {
		newObj.Finalizers = nil
		isNeedRemoveFinalizer = true
	}
	return
}

func (es *eniSyncher) Delete(name string) error {
	log.WithField(taskLogField, eniControllerName).
		Infof("eni(%s) have been deleted", name)
	eni, _ := es.updater.Lister().Get(name)
	if eni == nil {
		return nil
	}

	if es.mangeFinalizer(eni) {
		log.WithField("eni", eni.Name).Info("eni should remove finalizers")
		_, err := es.updater.Update(eni)
		if err != nil {
			log.WithError(err).Errorf("patch eni %s failed", eni.Name)
		}
		return err
	}
	return nil
}

func (es *eniSyncher) ResyncENI(context.Context) time.Duration {
	log.WithField(taskLogField, eniControllerName).Infof("start to resync eni")
	es.syncManager.RunImmediately()
	return es.resyncPeriod
}

// eniStateMachine ENI state machine, used to control the state flow of ENI
type eniStateMachine struct {
	es       *eniSyncher
	ctx      context.Context
	resource *ccev2.ENI
	vpceni   *eni.Eni
	scopeLog *logrus.Entry
	isSync   bool
}

// Start state machine flow
func (esm *eniStateMachine) start() error {
	// ENI for RDMA (ccev2.ENIForHPC or ccev2.ENIForERI) need do nothing, so return nil directly.
	// esm.es.remoteSyncer.statENI(esm.ctx, esm.resource.Name) can not stat ENI for RDMA, it will return
	// error like [Code: EniNotFoundException; Message: eni:eni-tzjatpp7gbh6 resource not exist;
	// RequestId: 148ca1d1-174f-494a-8192-5bae2a3bf0c7]". So we need to check ENI type first.
	if esm.resource.Spec.Type == ccev2.ENIForHPC || esm.resource.Spec.Type == ccev2.ENIForERI {
		return nil
	}
	var err error
	if esm.resource.Status.VPCStatus == ccev2.VPCENIStatusInuse {
		esm.scopeLog.Debugf("eni %s is already in inused", esm.resource.Name)
	} else if esm.resource.Status.VPCStatus != ccev2.VPCENIStatusDeleted {
		// TODO: The preCreated ENI's OwnerReferences maybe can be managed by instance-group in the feature.
		owners := esm.resource.OwnerReferences
		managedByMe := false
		if len(owners) != 0 {
			for _, o := range owners {
				if o.APIVersion == ccev2.SchemeGroupVersion.String() && o.Kind == ccev2.NRSKindDefinition {
					managedByMe = true
					break
				}
			}
		}
		if !managedByMe {
			esm.scopeLog.Infof("eni object %s has not been managed by cce-cni yet (OwnerReference not initialized), skip state machine",
				esm.resource.Name)
			esm.isSync = true
			return nil
		}

		// refresh status of ENI
		esm.vpceni, err = esm.es.remoteSyncer.statENI(esm.ctx, esm.resource.Name)
		if cloud.IsErrorReasonNoSuchObject(err) {
			// eni not found, will delete it which not inuse
			log.WithField("eniID", esm.resource.Name).Error("not inuse eni not found in vpc, will delete it")
			return esm.deleteENI()
		} else if err != nil {
			return fmt.Errorf("eni state machine failed to refresh eni(%s) status: %v", esm.resource.Name, err)
		}

		if esm.vpceni.Status == string(ccev2.VPCENIStatusInuse) {
			updateENISpecFromVPCENI(esm)
			// update spec
			var updateError error
			esm.resource, updateError = esm.es.updater.Update(esm.resource)
			if updateError != nil {
				esm.scopeLog.WithError(updateError).Error("update eni spec failed")
				return updateError
			}
			// add status
			(&esm.resource.Status).AppendVPCStatus(ccev2.VPCENIStatus(esm.vpceni.Status))
			esm.resource, err = esm.es.updater.UpdateStatus(esm.resource)
			if err != nil {
				return err
			}
			esm.isSync = true
			esm.scopeLog.Info("Successfully update the ENI status to inuse and populate its content.")
			return nil
		}

		switch esm.resource.Status.VPCStatus {
		case ccev2.VPCENIStatusAvailable:
			err = esm.attachENI()
		case ccev2.VPCENIStatusAttaching:
			err = esm.attachingENI()
		case ccev2.VPCENIStatusDetaching:
			// TODO: do nothing or try to delete eni
			// err = esm.deleteENI()
		}
		if err != nil {
			log.WithField(taskLogField, eniControllerName).
				WithContext(esm.ctx).
				WithError(err).
				Errorf("failed to run eni(%s) with status %s status machine", esm.resource.Spec.ENI.ID, esm.resource.Status.VPCStatus)
			return err
		}

		// refresh the status of ENI
		if esm.resource.Status.VPCStatus != ccev2.VPCENIStatus(esm.vpceni.Status) {
			(&esm.resource.Status).AppendVPCStatus(ccev2.VPCENIStatus(esm.vpceni.Status))
			return nil
		}

		// not the final status, will retry later
		return cm.NewDelayEvent(esm.resource.Name, ENIReadyTimeToAttach, fmt.Sprintf("eni %s status is not final: %s", esm.resource.Spec.ENI.ID, esm.resource.Status.VPCStatus))
	}
	return nil
}

func updateENISpecFromVPCENI(esm *eniStateMachine) {
	esm.resource.Spec.ENI.ID = esm.vpceni.EniId
	esm.resource.Spec.ENI.Name = esm.vpceni.Name
	esm.resource.Spec.ENI.MacAddress = esm.vpceni.MacAddress
	esm.resource.Spec.ENI.InstanceID = esm.vpceni.InstanceId
	esm.resource.Spec.ENI.SecurityGroupIds = esm.vpceni.SecurityGroupIds
	esm.resource.Spec.ENI.EnterpriseSecurityGroupIds = esm.vpceni.EnterpriseSecurityGroupIds
	esm.resource.Spec.ENI.Description = esm.vpceni.Description
	esm.resource.Spec.ENI.VpcID = esm.vpceni.VpcId
	esm.resource.Spec.ENI.ZoneName = esm.vpceni.ZoneName
	esm.resource.Spec.ENI.SubnetID = esm.vpceni.SubnetId
	esm.resource.Spec.ENI.PrivateIPSet = ToModelPrivateIP(esm.vpceni.PrivateIpSet, esm.vpceni.VpcId, esm.vpceni.SubnetId)
	esm.resource.Spec.ENI.IPV6PrivateIPSet = ToModelPrivateIP(esm.vpceni.Ipv6PrivateIpSet, esm.vpceni.VpcId, esm.vpceni.SubnetId)
	ElectENIIPv6PrimaryIP(esm.resource)
}

// attachENI attach a  ENI to instance
// Only accept calls whose ENI status is "available"
func (esm *eniStateMachine) attachENI() error {
	// status is not match
	if esm.vpceni.Status != string(ccev2.VPCENIStatusAvailable) {
		return nil
	}

	// eni is expired, do rollback
	if esm.resource.CreationTimestamp.Add(ENIMaxCreateDuration).Before(time.Now()) {
		return esm.deleteENI()
	}

	// try to attach eni to bcc instance
	err := esm.es.bceclient.AttachENI(esm.ctx, &enisdk.EniInstance{
		InstanceId: esm.resource.Spec.ENI.InstanceID,
		EniId:      esm.resource.Spec.ENI.ID,
	})
	if err != nil {
		esm.es.eventRecorder.Eventf(esm.resource, corev1.EventTypeWarning, "AttachENIFailed", "failed attach eni(%s) to %s, will delete it: %v", esm.resource.Spec.ENI.ID, esm.resource.Spec.ENI.InstanceID, err)

		err2 := esm.deleteENI()
		err = fmt.Errorf("failed to attach eni(%s) to instance(%s): %s, will delete eni crd", esm.resource.Spec.ENI.ID, esm.resource.Spec.ENI.InstanceID, err.Error())
		if err2 != nil {
			log.WithField("eniID", esm.resource.Name).Errorf("failed to delete eni crd: %v", err2)
		}
		return err
	}

	log.WithField(taskLogField, eniControllerName).
		WithContext(esm.ctx).
		Infof("attach eni(%s) to instance(%s) success", esm.resource.Spec.ENI.InstanceID, esm.resource.Spec.ENI.ID)
	return nil
}

// deleteENI roback to delete eni
func (esm *eniStateMachine) deleteENI() error {
	err := esm.es.bceclient.DeleteENI(esm.ctx, esm.resource.Spec.ENI.ID)
	if err != nil && !cloud.IsErrorReasonNoSuchObject(err) && !cloud.IsErrorENINotFound(err) {
		esm.es.eventRecorder.Eventf(esm.resource, corev1.EventTypeWarning, "DeleteENIFailed", "failed to delete eni(%s): %v", esm.resource.Spec.ENI.ID, err)
		return fmt.Errorf("failed to delete eni(%s): %s", esm.resource.Spec.ENI.ID, err.Error())
	}
	esm.es.eventRecorder.Eventf(esm.resource, corev1.EventTypeWarning, "DeleteENISuccess", "delete eni(%s) success", esm.resource.Spec.ENI.ID)
	// delete resource after delete eni in cloud
	err = esm.es.updater.Delete(esm.resource.Name)
	if err != nil {
		return fmt.Errorf("failed to delete eni(%s) crd resource: %s", esm.resource.Name, err.Error())
	}

	log.WithField("eniID", esm.resource.Name).Info("delete eni crd resource success")
	return nil
}

// attachingENI Processing ENI in the attaching state
// ENI may be stuck in the attaching state for a long time and need to be manually deleted
func (esm *eniStateMachine) attachingENI() error {
	// status is not match
	if esm.vpceni.Status != string(ccev2.VPCENIStatusAttaching) {
		return nil
	}

	if esm.resource.CreationTimestamp.Add(ENIMaxCreateDuration).Before(time.Now()) {
		esm.es.eventRecorder.Eventf(esm.resource, corev1.EventTypeWarning, "AttachingENIError", "eni(%s) is in attaching status more than %s, will delete it", esm.resource.Spec.ENI.ID, ENIMaxCreateDuration.String())
		return esm.deleteENI()
	}
	return nil
}

// ElectENIIPv6PrimaryIP elect a ipv6 primary ip for eni
// set primary ip for IPv6 if not set
// by default, all IPv6 IPs are secondary IPs
func ElectENIIPv6PrimaryIP(newObj *ccev2.ENI) {
	if len(newObj.Spec.ENI.IPV6PrivateIPSet) > 0 {
		if newObj.Annotations == nil {
			newObj.Annotations = make(map[string]string)
		}
		old := newObj.Annotations[k8s.AnnotationENIIPv6PrimaryIP]
		havePromaryIPv6 := false
		for _, ipv6PrivateIP := range newObj.Spec.ENI.IPV6PrivateIPSet {
			if ipv6PrivateIP.PrivateIPAddress == old {
				ipv6PrivateIP.Primary = true
				break
			}
			if ipv6PrivateIP.Primary {
				havePromaryIPv6 = true
				break
			}
		}
		if !havePromaryIPv6 {
			newObj.Spec.ENI.IPV6PrivateIPSet[0].Primary = true
			newObj.Annotations[k8s.AnnotationENIIPv6PrimaryIP] = newObj.Spec.ENI.IPV6PrivateIPSet[0].PrivateIPAddress
		}
	}
}

// ToModelPrivateIP convert private ip to model
func ToModelPrivateIP(ipset []enisdk.PrivateIp, vpcID, subnetID string) []*models.PrivateIP {
	var pIPSet []*models.PrivateIP
	for _, pip := range ipset {
		newPIP := &models.PrivateIP{
			PublicIPAddress:  pip.PublicIpAddress,
			PrivateIPAddress: pip.PrivateIpAddress,
			Primary:          pip.Primary,
		}
		newPIP.SubnetID = SearchSubnetID(vpcID, subnetID, pip.PrivateIpAddress)
		pIPSet = append(pIPSet, newPIP)
	}
	return pIPSet
}
