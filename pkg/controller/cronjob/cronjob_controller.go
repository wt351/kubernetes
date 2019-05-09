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

package cronjob

/*
I did not use watch or expectations.  Those add a lot of corner cases, and we aren't
expecting a large volume of jobs or scheduledJobs.  (We are favoring correctness
over scalability.  If we find a single controller thread is too slow because
there are a lot of Jobs or CronJobs, we can parallelize by Namespace.
If we find the load on the API server is too high, we can use a watch and
UndeltaStore.)

Just periodically list jobs and SJs, and then reconcile them.

*/

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/klog"

	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/pager"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/kubernetes/pkg/util/metrics"
)

// Utilities for dealing with Jobs and CronJobs and time.

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = batchv1beta1.SchemeGroupVersion.WithKind("CronJob")

// Controller is a controller for CronJobs.
type Controller struct {
	kubeClient clientset.Interface
	jobControl jobControlInterface //用于删除或增加job
	sjControl  sjControlInterface  //用于更新CronJob status
	podControl podControlInterface
	recorder   record.EventRecorder //时间记录器
}

// NewController creates and initializes a new Controller.
func NewController(kubeClient clientset.Interface) (*Controller, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	if kubeClient != nil && kubeClient.CoreV1().RESTClient().GetRateLimiter() != nil {
		if err := metrics.RegisterMetricAndTrackRateLimiterUsage("cronjob_controller", kubeClient.CoreV1().RESTClient().GetRateLimiter()); err != nil {
			return nil, err
		}
	}

	jm := &Controller{
		kubeClient: kubeClient,
		jobControl: realJobControl{KubeClient: kubeClient},
		sjControl:  &realSJControl{KubeClient: kubeClient},
		podControl: &realPodControl{KubeClient: kubeClient},
		recorder:   eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cronjob-controller"}),
	}

	return jm, nil
}

// Run starts the main goroutine responsible for watching and syncing jobs.
// 每10秒调用一次syncAll
func (jm *Controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	klog.Infof("Starting CronJob Manager")
	// Check things every 10 second.
	go wait.Until(jm.syncAll, 10*time.Second, stopCh)
	<-stopCh
	klog.Infof("Shutting down CronJob Manager")
}

// syncAll lists all the CronJobs and Jobs and reconciles them.
func (jm *Controller) syncAll() {
	// List children (Jobs) before parents (CronJob).
	// This guarantees that if we see any Job that got orphaned by the GC orphan finalizer,
	// we must also see that the parent CronJob has non-nil DeletionTimestamp (see #42639).
	// Note that this only works because we are NOT using any caches here.
	jobListFunc := func(opts metav1.ListOptions) (runtime.Object, error) {
		return jm.kubeClient.BatchV1().Jobs(metav1.NamespaceAll).List(opts)
	}

	// ListPager assists client code in breaking large list queries into multiple
	// smaller chunks of PageSize or smaller. PageFn is expected to accept a
	// metav1.ListOptions that supports paging and return a list. The pager does
	// not alter the field or label selectors on the initial options list.
	// pager用于协助client把大list查询分散成多个小块。
	// listPager.List返回整个list对象。但它会试图分批从api-server获取数据，以降低对api-server的影响。
	// 如果其中一个分批请求失败，List会去请求整个列表
	// options.Limit未设置，会使用默认的pageSize=500
	jlTmp, err := pager.New(pager.SimplePageFunc(jobListFunc)).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("can't list Jobs: %v", err))
		return
	}
	jl, ok := jlTmp.(*batchv1.JobList)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expected type *batchv1.JobList, got type %T", jlTmp))
		return
	}
	js := jl.Items
	klog.V(4).Infof("Found %d jobs", len(js))

	cronJobListFunc := func(opts metav1.ListOptions) (runtime.Object, error) {
		return jm.kubeClient.BatchV1beta1().CronJobs(metav1.NamespaceAll).List(opts)
	}
	sjlTmp, err := pager.New(pager.SimplePageFunc(cronJobListFunc)).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("can't list CronJobs: %v", err))
		return
	}
	sjl, ok := sjlTmp.(*batchv1beta1.CronJobList)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expected type *batchv1beta1.CronJobList, got type %T", sjlTmp))
		return
	}
	sjs := sjl.Items
	klog.V(4).Infof("Found %d cronjobs", len(sjs))

	//至此，从api-server获取了所有job的list和所有cronjob的list

	//对每个job调用getParentUIDFromJob(job),获取job的parent的UID.  job的parent只能是cronjob
	// 返回值jobsBySj的形式:
	//  [
	//    'apsopen/xxxx-cron' => [ jobObject1, jobObject2, jobObject3 ],
	//    'appshare/xxxx-cron' => [ jobObject4, jobObject5, jobObject6 ],
	//  ]
	jobsBySj := groupJobsByParent(js)
	klog.V(4).Infof("Found %d groups", len(jobsBySj))

	for _, sj := range sjs {
		// 同步某一cronJob(更新status,看是否要创建job，如果创建后，更新status)
		syncOne(&sj, jobsBySj[sj.UID], time.Now(), jm.jobControl, jm.sjControl, jm.recorder)
		// 根据SuccessfulJobsHistoryLimit和FailedJobsHistoryLimit这两个配置，删除job
		cleanupFinishedJobs(&sj, jobsBySj[sj.UID], jm.jobControl, jm.sjControl, jm.recorder)
	}
}

// cleanupFinishedJobs cleanups finished jobs created by a CronJob
func cleanupFinishedJobs(sj *batchv1beta1.CronJob, js []batchv1.Job, jc jobControlInterface,
	sjc sjControlInterface, recorder record.EventRecorder) {
	// If neither limits are active, there is no need to do anything.
	// 如果FailedJobsHistoryLimit和SuccessfulJobsHistoryLimit没有设置，那么不需要进行清理。
	if sj.Spec.FailedJobsHistoryLimit == nil && sj.Spec.SuccessfulJobsHistoryLimit == nil {
		return
	}

	failedJobs := []batchv1.Job{}
	succesfulJobs := []batchv1.Job{}

	for _, job := range js {
		isFinished, finishedStatus := getFinishedStatus(&job)
		if isFinished && finishedStatus == batchv1.JobComplete {
			succesfulJobs = append(succesfulJobs, job)
		} else if isFinished && finishedStatus == batchv1.JobFailed {
			failedJobs = append(failedJobs, job)
		}
	}

	//根据job创建时间升序进行清除(即使job正在执行也会去删除)
	if sj.Spec.SuccessfulJobsHistoryLimit != nil {
		removeOldestJobs(sj,
			succesfulJobs,
			jc,
			*sj.Spec.SuccessfulJobsHistoryLimit,
			recorder)
	}

	if sj.Spec.FailedJobsHistoryLimit != nil {
		removeOldestJobs(sj,
			failedJobs,
			jc,
			*sj.Spec.FailedJobsHistoryLimit,
			recorder)
	}

	// Update the CronJob, in case jobs were removed from the list.
	// 更新cronJob在apiserver中的状态
	if _, err := sjc.UpdateStatus(sj); err != nil {
		nameForLog := fmt.Sprintf("%s/%s", sj.Namespace, sj.Name)
		klog.Infof("Unable to update status for %s (rv = %s): %v", nameForLog, sj.ResourceVersion, err)
	}
}

// removeOldestJobs removes the oldest jobs from a list of jobs
func removeOldestJobs(sj *batchv1beta1.CronJob, js []batchv1.Job, jc jobControlInterface, maxJobs int32, recorder record.EventRecorder) {
	numToDelete := len(js) - int(maxJobs)
	if numToDelete <= 0 {
		return
	}

	nameForLog := fmt.Sprintf("%s/%s", sj.Namespace, sj.Name)
	klog.V(4).Infof("Cleaning up %d/%d jobs from %s", numToDelete, len(js), nameForLog)

	sort.Sort(byJobStartTime(js))
	for i := 0; i < numToDelete; i++ {
		klog.V(4).Infof("Removing job %s from %s", js[i].Name, nameForLog)
		deleteJob(sj, &js[i], jc, recorder)
	}
}

// syncOne reconciles a CronJob with a list of any Jobs that it created.
// All known jobs created by "sj" should be included in "js".
// The current time is passed in to facilitate testing.
// It has no receiver, to facilitate testing.
func syncOne(sj *batchv1beta1.CronJob, js []batchv1.Job, now time.Time, jc jobControlInterface, sjc sjControlInterface, recorder record.EventRecorder) {
	nameForLog := fmt.Sprintf("%s/%s", sj.Namespace, sj.Name)

	childrenJobs := make(map[types.UID]bool)
	for _, j := range js {
		childrenJobs[j.ObjectMeta.UID] = true
		// ActiveList存着running jobs的指针。 根据UID判断这个job是否在activeList中
		found := inActiveList(*sj, j.ObjectMeta.UID)
		if !found && !IsJobFinished(&j) { //job不在activeList且未完成。出错
			recorder.Eventf(sj, v1.EventTypeWarning, "UnexpectedJob", "Saw a job that the controller did not create or forgot: %s", j.Name)
			// We found an unfinished job that has us as the parent, but it is not in our Active list.
			// This could happen if we crashed right after creating the Job and before updating the status,
			// or if our jobs list is newer than our sj status after a relist, or if someone intentionally created
			// a job that they wanted us to adopt.

			// TODO: maybe handle the adoption case?  Concurrency/suspend rules will not apply in that case, obviously, since we can't
			// stop users from creating jobs if they have permission.  It is assumed that if a
			// user has permission to create a job within a namespace, then they have permission to make any scheduledJob
			// in the same namespace "adopt" that job.  ReplicaSets and their Pods work the same way.
			// TBS: how to update sj.Status.LastScheduleTime if the adopted job is newer than any we knew about?
		} else if found && IsJobFinished(&j) { //job在activeList但是已完成。从activeList里删除job
			_, status := getFinishedStatus(&j)
			deleteFromActiveList(sj, j.ObjectMeta.UID)
			recorder.Eventf(sj, v1.EventTypeNormal, "SawCompletedJob", "Saw completed job: %s, status: %v", j.Name, status)
		}
	}

	// Remove any job reference from the active list if the corresponding job does not exist any more.
	// Otherwise, the cronjob may be stuck in active mode forever even though there is no matching
	// job running.
	for _, j := range sj.Status.Active { //如果job不在activeList中。从activeList里删除这个job
		if found := childrenJobs[j.UID]; !found {
			recorder.Eventf(sj, v1.EventTypeNormal, "MissingJob", "Active job went missing: %v", j.Name)
			deleteFromActiveList(sj, j.UID)
		}
	}

	//更新api server里的状态
	updatedSJ, err := sjc.UpdateStatus(sj)
	if err != nil {
		klog.Errorf("Unable to update status for %s (rv = %s): %v", nameForLog, sj.ResourceVersion, err)
		return
	}
	*sj = *updatedSJ

	//这个cronjob即将被删除,更新完状态直接返回
	if sj.DeletionTimestamp != nil {
		// The CronJob is being deleted.
		// Don't do anything other than updating status.
		return
	}

	//sj.Spec.Suspend设为true，表示告诉这个controller后续不要再执行job。返回
	if sj.Spec.Suspend != nil && *sj.Spec.Suspend {
		klog.V(4).Infof("Not starting job for %s because it is suspended", nameForLog)
		return
	}

	//获得最近未成功启动的job
	//如果设定了StartingDeadlineSeconds，且now-StartingDeadlineSeconds大于最近一次成功调度时间和cronjob创建时间，那么在[now-StartingDeadlineSeconds,now]区间内获取最近未成功启动的job
	//如果没有设定StartingDeadlineSeconds或now-StartingDeadlineSeconds小于最近一次成功调度时间或cronjob创建时间，那么在[max(成功调度时间,creation time),now]区间内获取最近未成功启动的job
	//当最近未成功启动的job数量超过100,返回空切片和错误
	times, err := getRecentUnmetScheduleTimes(*sj, now)
	if err != nil {
		recorder.Eventf(sj, v1.EventTypeWarning, "FailedNeedsStart", "Cannot determine if job needs to be started: %v", err)
		klog.Errorf("Cannot determine if %s needs to be started: %v", nameForLog, err)
		return
	}
	// TODO: handle multiple unmet start times, from oldest to newest, updating status as needed.
	if len(times) == 0 {
		klog.V(4).Infof("No unmet start times for %s", nameForLog)
		return
	}
	if len(times) > 1 {
		klog.V(4).Infof("Multiple unmet start times for %s so only starting last one", nameForLog)
	}

	//提取最近一个应当调度的时间(可能是还未调度,可能是调度出错)
	scheduledTime := times[len(times)-1]

	//如果设置了StartingDeadlineSeconds，(最近应当调度时间+StartingDeadlineSeconds)<now，那么说明已经来不及调度了
	tooLate := false
	if sj.Spec.StartingDeadlineSeconds != nil {
		tooLate = scheduledTime.Add(time.Second * time.Duration(*sj.Spec.StartingDeadlineSeconds)).Before(now)
	}
	if tooLate {
		klog.V(4).Infof("Missed starting window for %s", nameForLog)
		recorder.Eventf(sj, v1.EventTypeWarning, "MissSchedule", "Missed scheduled time to start a job: %s", scheduledTime.Format(time.RFC1123Z))
		// TODO: Since we don't set LastScheduleTime when not scheduling, we are going to keep noticing
		// the miss every cycle.  In order to avoid sending multiple events, and to avoid processing
		// the sj again and again, we could set a Status.LastMissedTime when we notice a miss.
		// Then, when we call getRecentUnmetScheduleTimes, we can take max(creationTimestamp,
		// Status.LastScheduleTime, Status.LastMissedTime), and then so we won't generate
		// and event the next time we process it, and also so the user looking at the status
		// can see easily that there was a missed execution.
		return
	}

	//如果ConcurrencyPolicy是Forbid，且上一次调度还未完成(sj.Status.Active)数组不为空，返回，不执行
	if sj.Spec.ConcurrencyPolicy == batchv1beta1.ForbidConcurrent && len(sj.Status.Active) > 0 {
		// Regardless which source of information we use for the set of active jobs,
		// there is some risk that we won't see an active job when there is one.
		// (because we haven't seen the status update to the SJ or the created pod).
		// So it is theoretically possible to have concurrency with Forbid.
		// As long the as the invocations are "far enough apart in time", this usually won't happen.
		//
		// TODO: for Forbid, we could use the same name for every execution, as a lock.
		// With replace, we could use a name that is deterministic per execution time.
		// But that would mean that you could not inspect prior successes or failures of Forbid jobs.
		klog.V(4).Infof("Not starting job for %s because of prior execution still running and concurrency policy is Forbid", nameForLog)
		return
	}
	//如果ConcurrencyPolicy是Replace，删除这个cronjob所有在activeList的job并从activeList中删除，然后继续执行
	if sj.Spec.ConcurrencyPolicy == batchv1beta1.ReplaceConcurrent {
		for _, j := range sj.Status.Active {
			klog.V(4).Infof("Deleting job %s of %s that was still running at next scheduled start time", j.Name, nameForLog)

			job, err := jc.GetJob(j.Namespace, j.Name)
			if err != nil {
				recorder.Eventf(sj, v1.EventTypeWarning, "FailedGet", "Get job: %v", err)
				return
			}
			if !deleteJob(sj, job, jc, recorder) {
				return
			}
		}
	}

	//根据cronjob的template 创建一个job对象
	jobReq, err := getJobFromTemplate(sj, scheduledTime)
	if err != nil {
		klog.Errorf("Unable to make Job from template in %s: %v", nameForLog, err)
		return
	}

	//向apiserver发送请求，创建job
	jobResp, err := jc.CreateJob(sj.Namespace, jobReq)
	if err != nil {
		recorder.Eventf(sj, v1.EventTypeWarning, "FailedCreate", "Error creating job: %v", err)
		return
	}
	klog.V(4).Infof("Created Job %s for %s", jobResp.Name, nameForLog)
	recorder.Eventf(sj, v1.EventTypeNormal, "SuccessfulCreate", "Created job %v", jobResp.Name)

	// ------------------------------------------------------------------ //

	// If this process restarts at this point (after posting a job, but
	// before updating the status), then we might try to start the job on
	// the next time.  Actually, if we re-list the SJs and Jobs on the next
	// iteration of syncAll, we might not see our own status update, and
	// then post one again.  So, we need to use the job name as a lock to
	// prevent us from making the job twice (name the job with hash of its
	// scheduled time).

	// Add the just-started job to the status list.
	// 把新建的job加入到cronjob的activeList中，并更新最近成功调度时间。然后向apiserver更新cronjob的status
	ref, err := getRef(jobResp)
	if err != nil {
		klog.V(2).Infof("Unable to make object reference for job for %s", nameForLog)
	} else {
		sj.Status.Active = append(sj.Status.Active, *ref)
	}
	sj.Status.LastScheduleTime = &metav1.Time{Time: scheduledTime}
	if _, err := sjc.UpdateStatus(sj); err != nil {
		klog.Infof("Unable to update status for %s (rv = %s): %v", nameForLog, sj.ResourceVersion, err)
	}

	return
}

// deleteJob reaps a job, deleting the job, the pods and the reference in the active list
func deleteJob(sj *batchv1beta1.CronJob, job *batchv1.Job, jc jobControlInterface, recorder record.EventRecorder) bool {
	nameForLog := fmt.Sprintf("%s/%s", sj.Namespace, sj.Name)

	// delete the job itself...
	if err := jc.DeleteJob(job.Namespace, job.Name); err != nil {
		recorder.Eventf(sj, v1.EventTypeWarning, "FailedDelete", "Deleted job: %v", err)
		klog.Errorf("Error deleting job %s from %s: %v", job.Name, nameForLog, err)
		return false
	}
	// ... and its reference from active list
	deleteFromActiveList(sj, job.ObjectMeta.UID)
	recorder.Eventf(sj, v1.EventTypeNormal, "SuccessfulDelete", "Deleted job %v", job.Name)

	return true
}

func getRef(object runtime.Object) (*v1.ObjectReference, error) {
	return ref.GetReference(scheme.Scheme, object)
}
