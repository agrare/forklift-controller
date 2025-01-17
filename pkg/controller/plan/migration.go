package plan

import (
	"errors"
	"fmt"
	libcnd "github.com/konveyor/controller/pkg/condition"
	liberr "github.com/konveyor/controller/pkg/error"
	libitr "github.com/konveyor/controller/pkg/itinerary"
	api "github.com/konveyor/forklift-controller/pkg/apis/forklift/v1beta1"
	"github.com/konveyor/forklift-controller/pkg/apis/forklift/v1beta1/plan"
	"github.com/konveyor/forklift-controller/pkg/controller/plan/adapter"
	plancontext "github.com/konveyor/forklift-controller/pkg/controller/plan/context"
	"github.com/konveyor/forklift-controller/pkg/controller/plan/scheduler"
	"github.com/konveyor/forklift-controller/pkg/controller/provider/web"
	vmio "kubevirt.io/vm-import-operator/pkg/apis/v2v/v1beta1"
	"time"
)

//
// Requeue
const (
	NoReQ   = time.Duration(0)
	PollReQ = time.Second * 3
)

//
// Predicates.
var (
	HasPreHook  libitr.Flag = 0x01
	HasPostHook libitr.Flag = 0x02
)

//
// Phases.
const (
	Started       = "Started"
	PreHook       = "PreHook"
	CreateImport  = "CreateImport"
	ImportCreated = "ImportCreated"
	PostHook      = "PostHook"
	Completed     = "Completed"
)

//
// Steps.
const (
	DiskTransfer    = "DiskTransfer"
	ImageConversion = "ImageConversion"
)

var (
	itinerary = libitr.Itinerary{
		Name: "",
		Pipeline: libitr.Pipeline{
			{Name: Started},
			{Name: PreHook, All: HasPreHook},
			{Name: CreateImport},
			{Name: ImportCreated},
			{Name: PostHook, All: HasPostHook},
			{Name: Completed},
		},
	}
)

//
// Migration.
type Migration struct {
	*plancontext.Context
	// Builder
	builder adapter.Builder
	// kubevirt.
	kubevirt KubeVirt
	// VM import CRs.
	importMap ImportMap
	// VM scheduler
	scheduler scheduler.Scheduler
}

//
// Type of migration.
func (r *Migration) Type() string {
	return r.Context.Source.Provider.Type()
}

//
// Run the migration.
func (r *Migration) Run() (reQ time.Duration, err error) {
	reQ = PollReQ
	err = r.init()
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	err = r.begin()
	if err != nil {
		err = liberr.Wrap(err)
		return
	}

	r.resolveCanceledRefs()

	for _, vm := range r.runningVMs() {
		err = r.step(vm)
		if err != nil {
			return
		}
	}

	vm, hasNext, err := r.scheduler.Next()
	if err != nil {
		return
	}
	if hasNext {
		err = r.step(vm)
		if err != nil {
			return
		}
	}

	completed, err := r.end()
	if completed {
		reQ = NoReQ
	}

	return
}

//
// Steps a VM through the migration itinerary
// and updates its status.
func (r *Migration) step(vm *plan.VMStatus) (err error) {
	// check whether the VM has been canceled by the user
	if r.Context.Migration.Spec.Canceled(vm.Ref) {
		vm.SetCondition(
			libcnd.Condition{
				Type:     Canceled,
				Status:   True,
				Category: Advisory,
				Reason:   UserRequested,
				Message:  "The migration has been canceled.",
				Durable:  true,
			})
		vm.Phase = Completed
		r.Log.Info(
			"Migration [CANCELED]",
			"vm",
			vm.String())
		return
	}
	itinerary.Predicate = &Predicate{
		vm: &vm.VM,
	}

	r.Log.Info(
		"Migration [RUN]",
		"vm",
		vm.String(),
		"phase",
		vm.Phase)
	r.Log.V(2).Info(
		"Migrating VM (definition).",
		"vm",
		vm)

	switch vm.Phase {
	case Started:
		vm.MarkStarted()
		vm.Phase = r.next(vm.Phase)
	case PreHook, PostHook:
		runner := HookRunner{Context: r.Context}
		err = runner.Run(vm)
		if err != nil {
			return
		}
		if step, found := vm.FindStep(vm.Phase); found {
			if step.MarkedCompleted() && step.Error == nil {
				vm.Phase = r.next(vm.Phase)
			}
		} else {
			vm.Phase = Completed
		}
	case CreateImport:
		err = r.kubevirt.EnsureImport(vm)
		if err != nil {
			if !errors.As(err, &web.ProviderNotReadyError{}) {
				vm.AddError(err.Error())
				err = nil
				break
			} else {
				return
			}
		}
		vm.Phase = r.next(vm.Phase)
	case ImportCreated:
		// update the VM if the cutover
		// changed on the Migration
		err = r.kubevirt.EnsureImport(vm)
		if err != nil {
			if !errors.As(err, &web.ProviderNotReadyError{}) {
				vm.AddError(err.Error())
				err = nil
				break
			} else {
				return
			}
		}
		rErr := r.updateVM(vm)
		if rErr != nil {
			err = liberr.Wrap(rErr)
			return
		}
		// vSphere VMs require image conversion, other VMs are
		// complete after the disk transfer is finished.
		if step, found := vm.FindStep(ImageConversion); found {
			if step.MarkedCompleted() {
				if step.Error == nil {
					vm.Phase = r.next(vm.Phase)
				} else {
					vm.Phase = Completed
				}
			}
		} else if step, found = vm.FindStep(DiskTransfer); found {
			if step.MarkedCompleted() {
				if step.Error == nil {
					vm.Phase = r.next(vm.Phase)
				} else {
					vm.Phase = Completed
				}
			}
		}
	case Completed:
		vm.MarkCompleted()
		r.Log.Info(
			"Migration [COMPLETED]",
			"vm",
			vm.String())
	default:
		r.Log.Info(
			"Phase unknown.",
			"vm",
			vm)
		vm.Phase = Completed
		vm.AddError(
			fmt.Sprintf(
				"Phase [%s] unknown",
				vm.Phase))
	}
	vm.ReflectPipeline()
	if vm.Phase == Completed && vm.Error == nil {
		vm.SetCondition(
			libcnd.Condition{
				Type:     Succeeded,
				Status:   True,
				Category: Advisory,
				Message:  "The VM migration has SUCCEEDED.",
				Durable:  true,
			})
	} else if vm.Error != nil {
		vm.Phase = Completed
		vm.SetCondition(
			libcnd.Condition{
				Type:     Failed,
				Status:   True,
				Category: Advisory,
				Message:  "The VM migration has FAILED.",
				Durable:  true,
			})
	}
	return
}

//
// Cancel the migration.
// Delete resources associated with VMs that have failed or been marked canceled.
func (r *Migration) Cancel() (err error) {
	err = r.init()
	if err != nil {
		err = liberr.Wrap(err)
		return
	}

	for _, vm := range r.Plan.Status.Migration.VMs {
		if vm.HasAnyCondition(Canceled, Failed) {
			err = r.kubevirt.DeleteImport(vm)
			if err != nil {
				err = liberr.Wrap(err)
				return
			}
			vm.MarkCompleted()
			for _, step := range vm.Pipeline {
				if step.MarkedStarted() {
					step.MarkCompleted()
				}
			}
		}
	}

	return
}

//
// Best effort attempt to resolve canceled refs.
func (r *Migration) resolveCanceledRefs() {
	for i := range r.Context.Migration.Spec.Cancel {
		// resolve the VM ref in place
		ref := &r.Context.Migration.Spec.Cancel[i]
		_, _ = r.Source.Inventory.VM(ref)
	}
}

//
func (r *Migration) runningVMs() (vms []*plan.VMStatus) {
	vms = make([]*plan.VMStatus, 0)
	for i := range r.Plan.Status.Migration.VMs {
		vm := r.Plan.Status.Migration.VMs[i]
		if vm.Running() {
			vms = append(vms, vm)
		}
	}
	return
}

//
// Get/Build resources.
func (r *Migration) init() (err error) {
	adapter, err := adapter.New(r.Context.Source.Provider)
	if err != nil {
		return
	}

	r.builder, err = adapter.Builder(r.Context)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	r.kubevirt = KubeVirt{
		Context: r.Context,
		Builder: r.builder,
	}
	r.scheduler, err = scheduler.New(r.Context)
	if err != nil {
		return
	}

	return
}

//
// Next step in the itinerary.
func (r *Migration) next(phase string) (next string) {
	step, done, err := itinerary.Next(phase)
	if done || err != nil {
		next = Completed
		if err != nil {
			r.Log.Error(err, "Next phase failed.")
		}
	} else {
		next = step.Name
	}

	return
}

//
// Begin the migration.
func (r *Migration) begin() (err error) {
	snapshot := r.Plan.Status.Migration.ActiveSnapshot()
	if snapshot.HasAnyCondition(Executing, Succeeded, Failed, Canceled) {
		return
	}
	r.Plan.Status.Migration.MarkReset()
	r.Plan.Status.Migration.MarkStarted()
	snapshot.SetCondition(
		libcnd.Condition{
			Type:     Executing,
			Status:   True,
			Category: Advisory,
			Message:  "The plan is EXECUTING.",
			Durable:  true,
		})
	err = r.kubevirt.EnsureNamespace()
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	//
	// Delete
	kept := []*plan.VMStatus{}
	for _, status := range r.Plan.Status.Migration.VMs {

		// resolve the VM ref
		_, err = r.Source.Inventory.VM(&status.Ref)
		if err != nil {
			err = liberr.Wrap(err)
			return
		}

		if _, found := r.Plan.Spec.FindVM(status.Ref); found {
			kept = append(kept, status)
		}
	}
	r.Plan.Status.Migration.VMs = kept
	//
	// Add/Update.
	list := []*plan.VMStatus{}
	for _, vm := range r.Plan.Spec.VMs {
		var status *plan.VMStatus
		itinerary.Predicate = &Predicate{vm: &vm}
		step, _ := itinerary.First()
		if current, found := r.Plan.Status.Migration.FindVM(vm.Ref); !found {
			status = &plan.VMStatus{VM: vm}
		} else {
			status = current
		}
		if status.Phase != Completed || status.HasAnyCondition(Canceled, Failed) {
			pipeline, pErr := r.buildPipeline(&vm)
			if pErr != nil {
				err = liberr.Wrap(pErr)
				return
			}
			status.DeleteCondition(Canceled, Failed)
			status.MarkReset()
			status.Pipeline = pipeline
			status.Phase = step.Name
			status.Error = nil
			status.Warm = nil
			log.Info(
				"Pipeline reset.",
				"vm",
				vm.String())
		} else {
			log.Info(
				"Pipeline preserved.",
				"vm",
				vm.String())
		}
		list = append(list, status)
	}

	r.Plan.Status.Migration.VMs = list

	r.Log.Info("Migration [STARTED]")

	return
}

//
// Build the pipeline for a VM status.
func (r *Migration) buildPipeline(vm *plan.VM) (pipeline []*plan.Step, err error) {
	itinerary.Predicate = &Predicate{vm: vm}
	step, _ := itinerary.First()
	for {
		switch step.Name {
		case PreHook:
			pipeline = append(
				pipeline,
				&plan.Step{
					Task: plan.Task{
						Name:        PreHook,
						Description: "Run pre-migration hook.",
						Progress:    libitr.Progress{Total: 1},
					},
				})
		case CreateImport:
			tasks, pErr := r.builder.Tasks(vm.Ref)
			if pErr != nil {
				err = liberr.Wrap(pErr)
				return
			}
			total := int64(0)
			for _, task := range tasks {
				total += task.Progress.Total
			}
			pipeline = append(
				pipeline,
				&plan.Step{
					Task: plan.Task{
						Name:        DiskTransfer,
						Description: "Transfer disks.",
						Progress: libitr.Progress{
							Total: total,
						},
						Annotations: map[string]string{
							"unit": "MB",
						},
					},
					Tasks: tasks,
				})
			// only vSphere VMs require image conversion.
			if r.Source.Provider.Type() == api.VSphere {
				pipeline = append(
					pipeline,
					&plan.Step{
						Task: plan.Task{
							Name:        ImageConversion,
							Description: "Convert image to kubevirt.",
							Progress:    libitr.Progress{Total: 1},
						},
					})
			}
		case PostHook:
			pipeline = append(
				pipeline,
				&plan.Step{
					Task: plan.Task{
						Name:        PostHook,
						Description: "Run post-migration hook.",
						Progress:    libitr.Progress{Total: 1},
					},
				})
		}
		next, done, _ := itinerary.Next(step.Name)
		if !done {
			step = next
		} else {
			break
		}
	}

	log.V(2).Info(
		"Pipeline built.",
		"vm",
		vm.String())

	return
}

//
// End the migration.
func (r *Migration) end() (completed bool, err error) {
	failed := 0
	succeeded := 0
	for _, vm := range r.Plan.Status.Migration.VMs {
		if !vm.MarkedCompleted() {
			return
		}
		if vm.HasCondition(Failed) {
			failed++
		}
		if vm.HasCondition(Succeeded) {
			succeeded++
		}
	}
	r.Plan.Status.Migration.MarkCompleted()
	snapshot := r.Plan.Status.Migration.ActiveSnapshot()
	snapshot.DeleteCondition(Executing)

	if failed > 0 {
		// if any VMs failed, the migration failed.
		r.Log.Info("Migration [FAILED]")
		snapshot.SetCondition(
			libcnd.Condition{
				Type:     Failed,
				Status:   True,
				Category: Advisory,
				Message:  "The plan execution has FAILED.",
				Durable:  true,
			})
		err = r.Cancel()
		if err != nil {
			err = liberr.Wrap(err)
		}

	} else if succeeded > 0 {
		// if the migration didn't fail and at least one VM succeeded,
		// then the migration succeeded.
		r.Log.Info("Migration [SUCCEEDED]")
		snapshot.SetCondition(
			libcnd.Condition{
				Type:     Succeeded,
				Status:   True,
				Category: Advisory,
				Message:  "The plan execution has SUCCEEDED.",
				Durable:  true,
			})
	} else {
		// if there were no failures or successes, but
		// all the VMs are complete, then the migration must
		// have been canceled.
		r.Log.Info("Migration [CANCELED]")
		snapshot.SetCondition(
			libcnd.Condition{
				Type:     Canceled,
				Status:   True,
				Category: Advisory,
				Message:  "The plan execution has been CANCELED.",
				Durable:  true,
			})
	}

	completed = true
	return
}

//
// Update VM migration status.
func (r *Migration) updateVM(vm *plan.VMStatus) (err error) {
	if r.importMap == nil {
		r.importMap, err = r.kubevirt.ImportMap()
		if err != nil {
			err = liberr.Wrap(err)
			return
		}
	}
	var imp VmImport
	found := false
	if imp, found = r.importMap[vm.ID]; !found {
		msg := "Import CR not found."
		vm.AddError(msg)
		return
	}
	r.updatePipeline(vm, &imp)
	if imp.Spec.Warm {
		updateWarmStatus(vm, imp)
	}

	return
}

func updateWarmStatus(vm *plan.VMStatus, imp VmImport) {
	if vm.Warm == nil {
		vm.Warm = &plan.Warm{
			Precopies: make([]plan.Precopy, 0),
		}
	}
	vm.Warm.Successes = imp.Status.WarmImport.Successes
	vm.Warm.Failures = imp.Status.WarmImport.Failures
	vm.Warm.ConsecutiveFailures = imp.Status.WarmImport.ConsecutiveFailures
	vm.Warm.NextPrecopyAt = imp.Status.WarmImport.NextStageTime

	// Use VMI Processing condition transition times to figure
	// out the start and stop times of the precopies.
	conditions := imp.Conditions()
	cnd := conditions.FindCondition(string(vmio.Processing))
	if cnd != nil {
		switch cnd.Reason {
		case string(vmio.CopyingStage):
			if len(vm.Warm.Precopies) == 0 || vm.Warm.Precopies[len(vm.Warm.Precopies)-1].End != nil {
				vm.Warm.Precopies = append(vm.Warm.Precopies, plan.Precopy{Start: &cnd.LastTransitionTime})
			}
		case string(vmio.CopyingPaused):
			if len(vm.Warm.Precopies) != 0 && vm.Warm.Precopies[len(vm.Warm.Precopies)-1].End == nil {
				vm.Warm.Precopies[len(vm.Warm.Precopies)-1].End = &cnd.LastTransitionTime
			}
		}
	}
}

//
// Update the pipeline.
func (r *Migration) updatePipeline(vm *plan.VMStatus, imp *VmImport) {
	for _, step := range vm.Pipeline {
		if step.MarkedCompleted() {
			continue
		}
		switch step.Name {
		case DiskTransfer:
			var name string
			var task *plan.Task
			var tasksBlocked int
			var tasksCompleted int
			var tasksRunning int
		nextDv:
			for _, dv := range imp.DataVolumes {
				name = r.builder.ResolveDataVolumeIdentifier(dv.DataVolume)
				found := false
				task, found = step.FindTask(name)
				if !found {
					continue nextDv
				}
				conditions := dv.Conditions()
				cnd := conditions.FindCondition("Bound")
				if cnd != nil && cnd.Status == False {
					task.Phase = Blocked
					task.Reason = cnd.Reason
					tasksBlocked++
					continue nextDv
				}
				cnd = conditions.FindCondition("Running")
				if cnd == nil {
					continue nextDv
				}
				task.MarkStarted()
				task.Phase = Running
				task.Reason = cnd.Reason
				tasksRunning++
				pct := dv.PercentComplete()
				completed := pct * float64(task.Progress.Total)
				task.Progress.Completed = int64(completed)
				if conditions.HasCondition("Ready") {
					task.Progress.Completed = task.Progress.Total
					task.MarkCompleted()
					tasksCompleted++
				}
			}
			if tasksCompleted == len(step.Tasks) {
				step.Phase = Completed
			} else if tasksBlocked > 0 {
				step.Phase = Blocked
			} else if tasksRunning > 0 {
				step.Phase = Running
			}
		case ImageConversion:
			conditions := imp.Conditions()
			cnd := conditions.FindCondition("Processing")
			if cnd != nil {
				switch cnd.Reason {
				case string(vmio.Pending):
					vm.SetCondition(
						libcnd.Condition{
							Type:     Pending,
							Status:   True,
							Category: Advisory,
							Message:  "The VM migration is PENDING.",
						})
				case string(vmio.CopyingPaused):
					vm.SetCondition(
						libcnd.Condition{
							Type:     Paused,
							Status:   True,
							Category: Advisory,
							Message:  "The VM migration is PAUSED.",
						})
				case string(vmio.ConvertingGuest):
					if cnd.Status == True {
						step.MarkStarted()
					}
				}
				if step.MarkedStarted() {
					step.Phase = cnd.Reason
				}
			}
			pct := imp.PercentComplete()
			completed := pct * float64(step.Progress.Total)
			step.Progress.Completed = int64(completed)
			cnd = conditions.FindCondition("Succeeded")
			if cnd != nil {
				step.MarkCompleted()
				step.Progress.Completed = step.Progress.Total
				if cnd.Status != True {
					step.AddError(cnd.Message)
					step.Phase = cnd.Reason
				} else {
					step.Phase = Completed
				}
			}
		}
		step.ReflectTasks()
		if step.Error != nil {
			vm.AddError(step.Error.Reasons...)
		}
	}
}

//
// Step predicate.
type Predicate struct {
	// VM listed on the plan.
	vm *plan.VM
}

//
// Evaluate predicate flags.
func (r *Predicate) Evaluate(flag libitr.Flag) (allowed bool, err error) {
	if len(r.vm.Hooks) == 0 {
		return
	}
	switch flag {
	case HasPreHook:
		_, allowed = r.vm.FindHook(PreHook)
	case HasPostHook:
		_, allowed = r.vm.FindHook(PostHook)
	}

	return
}
