// Copyright 2018 The Kubeflow Authors
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

// Package controller provides a Kubernetes controller for a MXJob resource.
package mxnet

import (
	"fmt"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	mxv1 "github.com/kubeflow/mxnet-operator/pkg/apis/mxnet/v1"
	mxlogger "github.com/kubeflow/tf-operator/pkg/logger"
)

const (
	// mxJobCreatedReason is added in a mxjob when it is created.
	mxJobCreatedReason = "MXJobCreated"
	// mxJobSucceededReason is added in a mxjob when it is succeeded.
	mxJobSucceededReason = "MXJobSucceeded"
	// mxJobRunningReason is added in a mxjob when it is running.
	mxJobRunningReason = "MXJobRunning"
	// mxJobFailedReason is added in a mxjob when it is failed.
	mxJobFailedReason = "MXJobFailed"
	// mxJobRestarting is added in a mxjob when it is restarting.
	mxJobRestartingReason = "MXJobRestarting"
)

// updateStatus updates the status of the mxjob.
func (tc *MXController) updateStatusSingle(mxjob *mxv1.MXJob, rtype mxv1.MXReplicaType, replicas int, restart, schedulerCompleted bool) error {
	mxjobKey, err := KeyFunc(mxjob)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for mxjob object %#v: %v", mxjob, err))
		return err
	}

	// Expect to have `replicas - succeeded` pods alive.
	expected := replicas - int(mxjob.Status.MXReplicaStatuses[rtype].Succeeded)
	running := int(mxjob.Status.MXReplicaStatuses[rtype].Active)
	failed := int(mxjob.Status.MXReplicaStatuses[rtype].Failed)

	mxlogger.LoggerForJob(mxjob).Infof("MXJob=%s, ReplicaType=%s expected=%d, running=%d, failed=%d",
		mxjob.Name, rtype, expected, running, failed)
	// set StartTime.
	if mxjob.Status.StartTime == nil {
		now := metav1.Now()
		mxjob.Status.StartTime = &now
		// enqueue a sync to check if job past ActiveDeadlineSeconds
		if mxjob.Spec.ActiveDeadlineSeconds != nil {
			mxlogger.LoggerForJob(mxjob).Infof("Job with ActiveDeadlineSeconds will sync after %d seconds", *mxjob.Spec.ActiveDeadlineSeconds)
			tc.WorkQueue.AddAfter(mxjobKey, time.Duration(*mxjob.Spec.ActiveDeadlineSeconds)*time.Second)
		}
	}

	if ContainSchedulerSpec(mxjob) {
		if rtype == mxv1.MXReplicaTypeScheduler {
			if running > 0 {
				msg := fmt.Sprintf("MXJob %s is running.", mxjob.Name)
				err := updateMXJobConditions(mxjob, mxv1.MXJobRunning, mxJobRunningReason, msg)
				if err != nil {
					mxlogger.LoggerForJob(mxjob).Infof("Append mxjob condition error: %v", err)
					return err
				}
			}
			if expected == 0 {
				msg := fmt.Sprintf("MXJob %s is successfully completed.", mxjob.Name)
				if mxjob.Status.CompletionTime == nil {
					now := metav1.Now()
					mxjob.Status.CompletionTime = &now
				}
				err := updateMXJobConditions(mxjob, mxv1.MXJobSucceeded, mxJobSucceededReason, msg)
				if err != nil {
					mxlogger.LoggerForJob(mxjob).Infof("Append mxjob condition error: %v", err)
					return err
				}
			}
		}
	} else {
		if rtype == mxv1.MXReplicaTypeWorker || rtype == mxv1.MXReplicaTypeTuner {
			// All workers are succeeded or scheduler completed, leave a succeeded condition.
			if expected == 0 || schedulerCompleted {
				msg := fmt.Sprintf("MXJob %s is successfully completed.", mxjob.Name)
				if mxjob.Status.CompletionTime == nil {
					now := metav1.Now()
					mxjob.Status.CompletionTime = &now
				}
				err := updateMXJobConditions(mxjob, mxv1.MXJobSucceeded, mxJobSucceededReason, msg)
				if err != nil {
					mxlogger.LoggerForJob(mxjob).Infof("Append mxjob condition error: %v", err)
					return err
				}
			} else if running > 0 {
				// Some workers are still running, leave a running condition.
				msg := fmt.Sprintf("MXJob %s is running.", mxjob.Name)
				err := updateMXJobConditions(mxjob, mxv1.MXJobRunning, mxJobRunningReason, msg)
				if err != nil {
					mxlogger.LoggerForJob(mxjob).Infof("Append mxjob condition error: %v", err)
					return err
				}
			}
		}
	}

	if failed > 0 {
		if restart {
			msg := fmt.Sprintf("MXJob %s is restarting.", mxjob.Name)
			err := updateMXJobConditions(mxjob, mxv1.MXJobRestarting, mxJobRestartingReason, msg)
			if err != nil {
				mxlogger.LoggerForJob(mxjob).Infof("Append mxjob condition error: %v", err)
				return err
			}
		} else {
			msg := fmt.Sprintf("MXJob %s is failed.", mxjob.Name)
			if mxjob.Status.CompletionTime == nil {
				now := metav1.Now()
				mxjob.Status.CompletionTime = &now
			}
			err := updateMXJobConditions(mxjob, mxv1.MXJobFailed, mxJobFailedReason, msg)
			if err != nil {
				mxlogger.LoggerForJob(mxjob).Infof("Append mxjob condition error: %v", err)
				return err
			}
		}
	}
	return nil
}

// updateMXJobStatus updates the status of the given MXJob.
func (tc *MXController) updateMXJobStatus(mxjob *mxv1.MXJob) error {
	_, err := tc.mxJobClientSet.KubeflowV1().MXJobs(mxjob.Namespace).UpdateStatus(mxjob)
	return err
}

// updateMXJobConditions updates the conditions of the given mxjob.
func updateMXJobConditions(mxjob *mxv1.MXJob, conditionType mxv1.MXJobConditionType, reason, message string) error {
	condition := newCondition(conditionType, reason, message)
	setCondition(&mxjob.Status, condition)
	return nil
}

// initializeMXReplicaStatuses initializes the MXReplicaStatuses for replica.
func initializeMXReplicaStatuses(mxjob *mxv1.MXJob, rtype mxv1.MXReplicaType) {
	if mxjob.Status.MXReplicaStatuses == nil {
		mxjob.Status.MXReplicaStatuses = make(map[mxv1.MXReplicaType]*mxv1.MXReplicaStatus)
	}

	mxjob.Status.MXReplicaStatuses[rtype] = &mxv1.MXReplicaStatus{}
}

// updateMXJobReplicaStatuses updates the MXJobReplicaStatuses according to the pod.
func updateMXJobReplicaStatuses(mxjob *mxv1.MXJob, rtype mxv1.MXReplicaType, pod *v1.Pod) {
	switch pod.Status.Phase {
	case v1.PodRunning:
		mxjob.Status.MXReplicaStatuses[rtype].Active++
	case v1.PodSucceeded:
		mxjob.Status.MXReplicaStatuses[rtype].Succeeded++
	case v1.PodFailed:
		mxjob.Status.MXReplicaStatuses[rtype].Failed++
	}
}

// newCondition creates a new mxjob condition.
func newCondition(conditionType mxv1.MXJobConditionType, reason, message string) mxv1.MXJobCondition {
	return mxv1.MXJobCondition{
		Type:               conditionType,
		Status:             v1.ConditionTrue,
		LastUpdateTime:     metav1.Now(),
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

// getCondition returns the condition with the provided type.
func getCondition(status mxv1.MXJobStatus, condType mxv1.MXJobConditionType) *mxv1.MXJobCondition {
	if len(status.Conditions) > 0 {
		return &status.Conditions[len(status.Conditions)-1]
	}
	return nil
}

func hasCondition(status mxv1.MXJobStatus, condType mxv1.MXJobConditionType) bool {
	for _, condition := range status.Conditions {
		if condition.Type == condType && condition.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func isSucceeded(status mxv1.MXJobStatus) bool {
	return hasCondition(status, mxv1.MXJobSucceeded)
}

func isFailed(status mxv1.MXJobStatus) bool {
	return hasCondition(status, mxv1.MXJobFailed)
}

// setCondition updates the mxjob to include the provided condition.
// If the condition that we are about to add already exists
// and has the same status and reason then we are not going to update.
func setCondition(status *mxv1.MXJobStatus, condition mxv1.MXJobCondition) {
	// Do nothing if MXJobStatus is completed
	if isFailed(*status) || isSucceeded(*status) {
		return
	}

	currentCond := getCondition(*status, condition.Type)

	// Do nothing if condition doesn't change
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason {
		return
	}

	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}

	// Append the updated condition to the
	newConditions := filterOutCondition(status.Conditions, condition.Type)
	status.Conditions = append(newConditions, condition)
}

// filterOutCondition returns a new slice of mxjob conditions without conditions with the provided type.
func filterOutCondition(conditions []mxv1.MXJobCondition, condType mxv1.MXJobConditionType) []mxv1.MXJobCondition {
	var newConditions []mxv1.MXJobCondition
	for _, c := range conditions {
		if condType == mxv1.MXJobRestarting && c.Type == mxv1.MXJobRunning {
			continue
		}
		if condType == mxv1.MXJobRunning && c.Type == mxv1.MXJobRestarting {
			continue
		}

		if c.Type == condType {
			continue
		}

		// Set the running condition status to be false when current condition failed or succeeded
		if (condType == mxv1.MXJobFailed || condType == mxv1.MXJobSucceeded) && c.Type == mxv1.MXJobRunning {
			c.Status = v1.ConditionFalse
		}

		newConditions = append(newConditions, c)
	}
	return newConditions
}
