package scheduler

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/go-events"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/identity"
	"github.com/docker/swarmkit/manager/state"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
)

func TestScheduler(t *testing.T) {
	ctx := context.Background()
	initialNodeSet := []*api.Node{
		{
			ID: "id1",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name1",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
		{
			ID: "id2",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name2",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
		{
			ID: "id3",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name2",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
	}

	initialTaskSet := []*api.Task{
		{
			ID:           "id1",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name1",
			},

			Status: api.TaskStatus{
				State: api.TaskStateAssigned,
			},
			NodeID: initialNodeSet[0].ID,
		},
		{
			ID:           "id2",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name2",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		},
		{
			ID:           "id3",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name2",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		},
	}

	s := store.NewMemoryStore(nil)
	assert.NotNil(t, s)
	defer s.Close()

	err := s.Update(func(tx store.Tx) error {
		// Prepoulate nodes
		for _, n := range initialNodeSet {
			assert.NoError(t, store.CreateNode(tx, n))
		}

		// Prepopulate tasks
		for _, task := range initialTaskSet {
			assert.NoError(t, store.CreateTask(tx, task))
		}
		return nil
	})
	assert.NoError(t, err)

	scheduler := New(s)

	watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})
	defer cancel()

	go func() {
		assert.NoError(t, scheduler.Run(ctx))
	}()
	defer scheduler.Stop()

	assignment1 := watchAssignment(t, watch)
	// must assign to id2 or id3 since id1 already has a task
	assert.Regexp(t, assignment1.NodeID, "(id2|id3)")

	assignment2 := watchAssignment(t, watch)
	// must assign to id2 or id3 since id1 already has a task
	if assignment1.NodeID == "id2" {
		assert.Equal(t, "id3", assignment2.NodeID)
	} else {
		assert.Equal(t, "id2", assignment2.NodeID)
	}

	err = s.Update(func(tx store.Tx) error {
		// Update each node to make sure this doesn't mess up the
		// scheduler's state.
		for _, n := range initialNodeSet {
			assert.NoError(t, store.UpdateNode(tx, n))
		}
		return nil
	})
	assert.NoError(t, err)

	err = s.Update(func(tx store.Tx) error {
		// Delete the task associated with node 1 so it's now the most lightly
		// loaded node.
		assert.NoError(t, store.DeleteTask(tx, "id1"))

		// Create a new task. It should get assigned to id1.
		t4 := &api.Task{
			ID:           "id4",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name4",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		}
		assert.NoError(t, store.CreateTask(tx, t4))
		return nil
	})
	assert.NoError(t, err)

	assignment3 := watchAssignment(t, watch)
	assert.Equal(t, "id1", assignment3.NodeID)

	// Update a task to make it unassigned. It should get assigned by the
	// scheduler.
	err = s.Update(func(tx store.Tx) error {
		// Remove assignment from task id4. It should get assigned
		// to node id1.
		t4 := &api.Task{
			ID:           "id4",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name4",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		}
		assert.NoError(t, store.UpdateTask(tx, t4))
		return nil
	})
	assert.NoError(t, err)

	assignment4 := watchAssignment(t, watch)
	assert.Equal(t, "id1", assignment4.NodeID)

	err = s.Update(func(tx store.Tx) error {
		// Create a ready node, then remove it. No tasks should ever
		// be assigned to it.
		node := &api.Node{
			ID: "removednode",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "removednode",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_DOWN,
			},
		}
		assert.NoError(t, store.CreateNode(tx, node))
		assert.NoError(t, store.DeleteNode(tx, node.ID))

		// Create an unassigned task.
		task := &api.Task{
			ID:           "removednode",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "removednode",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		}
		assert.NoError(t, store.CreateTask(tx, task))
		return nil
	})
	assert.NoError(t, err)

	assignmentRemovedNode := watchAssignment(t, watch)
	assert.NotEqual(t, "removednode", assignmentRemovedNode.NodeID)

	err = s.Update(func(tx store.Tx) error {
		// Create a ready node. It should be used for the next
		// assignment.
		n4 := &api.Node{
			ID: "id4",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name4",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		}
		assert.NoError(t, store.CreateNode(tx, n4))

		// Create an unassigned task.
		t5 := &api.Task{
			ID:           "id5",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name5",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		}
		assert.NoError(t, store.CreateTask(tx, t5))
		return nil
	})
	assert.NoError(t, err)

	assignment5 := watchAssignment(t, watch)
	assert.Equal(t, "id4", assignment5.NodeID)

	err = s.Update(func(tx store.Tx) error {
		// Create a non-ready node. It should NOT be used for the next
		// assignment.
		n5 := &api.Node{
			ID: "id5",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name5",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_DOWN,
			},
		}
		assert.NoError(t, store.CreateNode(tx, n5))

		// Create an unassigned task.
		t6 := &api.Task{
			ID:           "id6",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name6",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		}
		assert.NoError(t, store.CreateTask(tx, t6))
		return nil
	})
	assert.NoError(t, err)

	assignment6 := watchAssignment(t, watch)
	assert.NotEqual(t, "id5", assignment6.NodeID)

	err = s.Update(func(tx store.Tx) error {
		// Update node id5 to put it in the READY state.
		n5 := &api.Node{
			ID: "id5",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name5",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		}
		assert.NoError(t, store.UpdateNode(tx, n5))

		// Create an unassigned task. Should be assigned to the
		// now-ready node.
		t7 := &api.Task{
			ID:           "id7",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name7",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		}
		assert.NoError(t, store.CreateTask(tx, t7))
		return nil
	})
	assert.NoError(t, err)

	assignment7 := watchAssignment(t, watch)
	assert.Equal(t, "id5", assignment7.NodeID)

	err = s.Update(func(tx store.Tx) error {
		// Create a ready node, then immediately take it down. The next
		// unassigned task should NOT be assigned to it.
		n6 := &api.Node{
			ID: "id6",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name6",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		}
		assert.NoError(t, store.CreateNode(tx, n6))
		n6.Status.State = api.NodeStatus_DOWN
		assert.NoError(t, store.UpdateNode(tx, n6))

		// Create an unassigned task.
		t8 := &api.Task{
			ID:           "id8",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name8",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		}
		assert.NoError(t, store.CreateTask(tx, t8))
		return nil
	})
	assert.NoError(t, err)

	assignment8 := watchAssignment(t, watch)
	assert.NotEqual(t, "id6", assignment8.NodeID)
}

func TestHA(t *testing.T) {
	ctx := context.Background()
	initialNodeSet := []*api.Node{
		{
			ID: "id1",
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
		{
			ID: "id2",
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
		{
			ID: "id3",
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
		{
			ID: "id4",
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
		{
			ID: "id5",
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
	}

	taskTemplate1 := &api.Task{
		DesiredState: api.TaskStateRunning,
		ServiceID:    "service1",
		Spec: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Image: "v:1",
				},
			},
		},
		Status: api.TaskStatus{
			State: api.TaskStateAllocated,
		},
	}

	taskTemplate2 := &api.Task{
		DesiredState: api.TaskStateRunning,
		ServiceID:    "service2",
		Spec: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Image: "v:2",
				},
			},
		},
		Status: api.TaskStatus{
			State: api.TaskStateAllocated,
		},
	}

	s := store.NewMemoryStore(nil)
	assert.NotNil(t, s)
	defer s.Close()

	t1Instances := 18

	err := s.Update(func(tx store.Tx) error {
		// Prepoulate nodes
		for _, n := range initialNodeSet {
			assert.NoError(t, store.CreateNode(tx, n))
		}

		// Prepopulate tasks from template 1
		for i := 0; i != t1Instances; i++ {
			taskTemplate1.ID = fmt.Sprintf("t1id%d", i)
			assert.NoError(t, store.CreateTask(tx, taskTemplate1))
		}
		return nil
	})
	assert.NoError(t, err)

	scheduler := New(s)

	watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})
	defer cancel()

	go func() {
		assert.NoError(t, scheduler.Run(ctx))
	}()
	defer scheduler.Stop()

	t1Assignments := make(map[string]int)
	for i := 0; i != t1Instances; i++ {
		assignment := watchAssignment(t, watch)
		if !strings.HasPrefix(assignment.ID, "t1") {
			t.Fatal("got assignment for different kind of task")
		}
		t1Assignments[assignment.NodeID]++
	}

	assert.Len(t, t1Assignments, 5)

	nodesWith3T1Tasks := 0
	nodesWith4T1Tasks := 0
	for nodeID, taskCount := range t1Assignments {
		if taskCount == 3 {
			nodesWith3T1Tasks++
		} else if taskCount == 4 {
			nodesWith4T1Tasks++
		} else {
			t.Fatalf("unexpected number of tasks %d on node %s", taskCount, nodeID)
		}
	}

	assert.Equal(t, 3, nodesWith4T1Tasks)
	assert.Equal(t, 2, nodesWith3T1Tasks)

	t2Instances := 2

	// Add a new service with two instances. They should fill the nodes
	// that only have two tasks.
	err = s.Update(func(tx store.Tx) error {
		for i := 0; i != t2Instances; i++ {
			taskTemplate2.ID = fmt.Sprintf("t2id%d", i)
			assert.NoError(t, store.CreateTask(tx, taskTemplate2))
		}
		return nil
	})
	assert.NoError(t, err)

	t2Assignments := make(map[string]int)
	for i := 0; i != t2Instances; i++ {
		assignment := watchAssignment(t, watch)
		if !strings.HasPrefix(assignment.ID, "t2") {
			t.Fatal("got assignment for different kind of task")
		}
		t2Assignments[assignment.NodeID]++
	}

	assert.Len(t, t2Assignments, 2)

	for nodeID := range t2Assignments {
		assert.Equal(t, 3, t1Assignments[nodeID])
	}

	// Scale up service 1 to 21 tasks. It should cover the two nodes that
	// service 2 was assigned to, and also one other node.
	err = s.Update(func(tx store.Tx) error {
		for i := t1Instances; i != t1Instances+3; i++ {
			taskTemplate1.ID = fmt.Sprintf("t1id%d", i)
			assert.NoError(t, store.CreateTask(tx, taskTemplate1))
		}
		return nil
	})
	assert.NoError(t, err)

	var sharedNodes [2]string

	for i := 0; i != 3; i++ {
		assignment := watchAssignment(t, watch)
		if !strings.HasPrefix(assignment.ID, "t1") {
			t.Fatal("got assignment for different kind of task")
		}
		if t1Assignments[assignment.NodeID] == 5 {
			t.Fatal("more than one new task assigned to the same node")
		}
		t1Assignments[assignment.NodeID]++

		if t2Assignments[assignment.NodeID] != 0 {
			if sharedNodes[0] == "" {
				sharedNodes[0] = assignment.NodeID
			} else if sharedNodes[1] == "" {
				sharedNodes[1] = assignment.NodeID
			} else {
				t.Fatal("all three assignments went to nodes with service2 tasks")
			}
		}
	}

	assert.NotEmpty(t, sharedNodes[0])
	assert.NotEmpty(t, sharedNodes[1])
	assert.NotEqual(t, sharedNodes[0], sharedNodes[1])

	nodesWith4T1Tasks = 0
	nodesWith5T1Tasks := 0
	for nodeID, taskCount := range t1Assignments {
		if taskCount == 4 {
			nodesWith4T1Tasks++
		} else if taskCount == 5 {
			nodesWith5T1Tasks++
		} else {
			t.Fatalf("unexpected number of tasks %d on node %s", taskCount, nodeID)
		}
	}

	assert.Equal(t, 4, nodesWith4T1Tasks)
	assert.Equal(t, 1, nodesWith5T1Tasks)

	// Add another task from service2. It must not land on the node that
	// has 5 service1 tasks.
	err = s.Update(func(tx store.Tx) error {
		taskTemplate2.ID = "t2id4"
		assert.NoError(t, store.CreateTask(tx, taskTemplate2))
		return nil
	})
	assert.NoError(t, err)

	assignment := watchAssignment(t, watch)
	if assignment.ID != "t2id4" {
		t.Fatal("got assignment for different task")
	}

	if t2Assignments[assignment.NodeID] != 0 {
		t.Fatal("was scheduled on a node that already has a service2 task")
	}
	if t1Assignments[assignment.NodeID] == 5 {
		t.Fatal("was scheduled on the node that has the most service1 tasks")
	}
	t2Assignments[assignment.NodeID]++

	// Remove all tasks on node id1.
	err = s.Update(func(tx store.Tx) error {
		tasks, err := store.FindTasks(tx, store.ByNodeID("id1"))
		assert.NoError(t, err)
		for _, task := range tasks {
			assert.NoError(t, store.DeleteTask(tx, task.ID))
		}
		return nil
	})
	assert.NoError(t, err)

	t1Assignments["id1"] = 0
	t2Assignments["id1"] = 0

	// Add four instances of service1 and two instances of service2.
	// All instances of service1 should land on node "id1", and one
	// of the two service2 instances should as well.
	// Put these in a map to randomize the order in which they are
	// created.
	err = s.Update(func(tx store.Tx) error {
		tasksMap := make(map[string]*api.Task)
		for i := 22; i <= 25; i++ {
			taskTemplate1.ID = fmt.Sprintf("t1id%d", i)
			tasksMap[taskTemplate1.ID] = taskTemplate1.Copy()
		}
		for i := 5; i <= 6; i++ {
			taskTemplate2.ID = fmt.Sprintf("t2id%d", i)
			tasksMap[taskTemplate2.ID] = taskTemplate2.Copy()
		}
		for _, task := range tasksMap {
			assert.NoError(t, store.CreateTask(tx, task))
		}
		return nil
	})
	assert.NoError(t, err)

	for i := 0; i != 4+2; i++ {
		assignment := watchAssignment(t, watch)
		if strings.HasPrefix(assignment.ID, "t1") {
			t1Assignments[assignment.NodeID]++
		} else if strings.HasPrefix(assignment.ID, "t2") {
			t2Assignments[assignment.NodeID]++
		}
	}

	assert.Equal(t, 4, t1Assignments["id1"])
	assert.Equal(t, 1, t2Assignments["id1"])
}

func TestSchedulerNoReadyNodes(t *testing.T) {
	ctx := context.Background()
	initialTask := &api.Task{
		ID:           "id1",
		DesiredState: api.TaskStateRunning,
		ServiceAnnotations: api.Annotations{
			Name: "name1",
		},
		Status: api.TaskStatus{
			State: api.TaskStateAllocated,
		},
	}

	s := store.NewMemoryStore(nil)
	assert.NotNil(t, s)
	defer s.Close()

	err := s.Update(func(tx store.Tx) error {
		// Add initial task
		assert.NoError(t, store.CreateTask(tx, initialTask))
		return nil
	})
	assert.NoError(t, err)

	scheduler := New(s)

	watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})
	defer cancel()

	go func() {
		assert.NoError(t, scheduler.Run(ctx))
	}()
	defer scheduler.Stop()

	err = s.Update(func(tx store.Tx) error {
		// Create a ready node. The task should get assigned to this
		// node.
		node := &api.Node{
			ID: "newnode",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "newnode",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		}
		assert.NoError(t, store.CreateNode(tx, node))
		return nil
	})
	assert.NoError(t, err)

	assignment := watchAssignment(t, watch)
	assert.Equal(t, "newnode", assignment.NodeID)
}

func TestSchedulerResourceConstraint(t *testing.T) {
	ctx := context.Background()
	// Create a ready node without enough memory to run the task.
	underprovisionedNode := &api.Node{
		ID: "underprovisioned",
		Spec: api.NodeSpec{
			Annotations: api.Annotations{
				Name: "underprovisioned",
			},
		},
		Status: api.NodeStatus{
			State: api.NodeStatus_READY,
		},
		Description: &api.NodeDescription{
			Resources: &api.Resources{
				NanoCPUs:    1e9,
				MemoryBytes: 1e9,
			},
		},
	}

	initialTask := &api.Task{
		ID:           "id1",
		DesiredState: api.TaskStateRunning,
		Spec: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{},
			},
			Resources: &api.ResourceRequirements{
				Reservations: &api.Resources{
					MemoryBytes: 2e9,
				},
			},
		},
		ServiceAnnotations: api.Annotations{
			Name: "name1",
		},
		Status: api.TaskStatus{
			State: api.TaskStateAllocated,
		},
	}

	s := store.NewMemoryStore(nil)
	assert.NotNil(t, s)
	defer s.Close()

	err := s.Update(func(tx store.Tx) error {
		// Add initial node and task
		assert.NoError(t, store.CreateTask(tx, initialTask))
		assert.NoError(t, store.CreateNode(tx, underprovisionedNode))
		return nil
	})
	assert.NoError(t, err)

	scheduler := New(s)

	watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})
	defer cancel()

	go func() {
		assert.NoError(t, scheduler.Run(ctx))
	}()
	defer scheduler.Stop()

	err = s.Update(func(tx store.Tx) error {
		// Create a node with enough memory. The task should get
		// assigned to this node.
		node := &api.Node{
			ID: "bignode",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "bignode",
				},
			},
			Description: &api.NodeDescription{
				Resources: &api.Resources{
					NanoCPUs:    4e9,
					MemoryBytes: 8e9,
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		}
		assert.NoError(t, store.CreateNode(tx, node))
		return nil
	})
	assert.NoError(t, err)

	assignment := watchAssignment(t, watch)
	assert.Equal(t, "bignode", assignment.NodeID)
}

func TestSchedulerResourceConstraintDeadTask(t *testing.T) {
	ctx := context.Background()
	// Create a ready node without enough memory to run the task.
	node := &api.Node{
		ID: "id1",
		Spec: api.NodeSpec{
			Annotations: api.Annotations{
				Name: "node",
			},
		},
		Status: api.NodeStatus{
			State: api.NodeStatus_READY,
		},
		Description: &api.NodeDescription{
			Resources: &api.Resources{
				NanoCPUs:    1e9,
				MemoryBytes: 1e9,
			},
		},
	}

	bigTask1 := &api.Task{
		DesiredState: api.TaskStateRunning,
		ID:           "id1",
		Spec: api.TaskSpec{
			Resources: &api.ResourceRequirements{
				Reservations: &api.Resources{
					MemoryBytes: 8e8,
				},
			},
		},
		ServiceAnnotations: api.Annotations{
			Name: "big",
		},
		Status: api.TaskStatus{
			State: api.TaskStateAllocated,
		},
	}

	bigTask2 := bigTask1.Copy()
	bigTask2.ID = "id2"

	s := store.NewMemoryStore(nil)
	assert.NotNil(t, s)
	defer s.Close()

	err := s.Update(func(tx store.Tx) error {
		// Add initial node and task
		assert.NoError(t, store.CreateNode(tx, node))
		assert.NoError(t, store.CreateTask(tx, bigTask1))
		return nil
	})
	assert.NoError(t, err)

	scheduler := New(s)

	watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})
	defer cancel()

	go func() {
		assert.NoError(t, scheduler.Run(ctx))
	}()
	defer scheduler.Stop()

	// The task fits, so it should get assigned
	assignment := watchAssignment(t, watch)
	assert.Equal(t, "id1", assignment.ID)
	assert.Equal(t, "id1", assignment.NodeID)

	err = s.Update(func(tx store.Tx) error {
		// Add a second task. It shouldn't get assigned because of
		// resource constraints.
		return store.CreateTask(tx, bigTask2)
	})
	assert.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	s.View(func(tx store.ReadTx) {
		tasks, err := store.FindTasks(tx, store.ByNodeID(node.ID))
		assert.NoError(t, err)
		assert.Len(t, tasks, 1)
	})

	err = s.Update(func(tx store.Tx) error {
		// The task becomes dead
		updatedTask := store.GetTask(tx, bigTask1.ID)
		updatedTask.Status.State = api.TaskStateShutdown
		return store.UpdateTask(tx, updatedTask)
	})
	assert.NoError(t, err)

	// With the first task no longer consuming resources, the second
	// one can be scheduled.
	assignment = watchAssignment(t, watch)
	assert.Equal(t, "id2", assignment.ID)
	assert.Equal(t, "id1", assignment.NodeID)
}

func TestSchedulerPreexistingDeadTask(t *testing.T) {
	ctx := context.Background()
	// Create a ready node without enough memory to run two tasks at once.
	node := &api.Node{
		ID: "id1",
		Spec: api.NodeSpec{
			Annotations: api.Annotations{
				Name: "node",
			},
		},
		Status: api.NodeStatus{
			State: api.NodeStatus_READY,
		},
		Description: &api.NodeDescription{
			Resources: &api.Resources{
				NanoCPUs:    1e9,
				MemoryBytes: 1e9,
			},
		},
	}

	deadTask := &api.Task{
		DesiredState: api.TaskStateRunning,
		ID:           "id1",
		NodeID:       "id1",
		Spec: api.TaskSpec{
			Resources: &api.ResourceRequirements{
				Reservations: &api.Resources{
					MemoryBytes: 8e8,
				},
			},
		},
		ServiceAnnotations: api.Annotations{
			Name: "big",
		},
		Status: api.TaskStatus{
			State: api.TaskStateShutdown,
		},
	}

	bigTask2 := deadTask.Copy()
	bigTask2.ID = "id2"
	bigTask2.Status.State = api.TaskStateAllocated

	s := store.NewMemoryStore(nil)
	assert.NotNil(t, s)
	defer s.Close()

	err := s.Update(func(tx store.Tx) error {
		// Add initial node and task
		assert.NoError(t, store.CreateNode(tx, node))
		assert.NoError(t, store.CreateTask(tx, deadTask))
		return nil
	})
	assert.NoError(t, err)

	scheduler := New(s)

	watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})
	defer cancel()

	go func() {
		assert.NoError(t, scheduler.Run(ctx))
	}()
	defer scheduler.Stop()

	err = s.Update(func(tx store.Tx) error {
		// Add a second task. It should get assigned because the task
		// using the resources is past the running state.
		return store.CreateTask(tx, bigTask2)
	})
	assert.NoError(t, err)

	assignment := watchAssignment(t, watch)
	assert.Equal(t, "id2", assignment.ID)
	assert.Equal(t, "id1", assignment.NodeID)
}

func TestPreassignedTasks(t *testing.T) {
	ctx := context.Background()
	initialNodeSet := []*api.Node{
		{
			ID: "node1",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name1",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
		{
			ID: "node2",
			Spec: api.NodeSpec{
				Annotations: api.Annotations{
					Name: "name2",
				},
			},
			Status: api.NodeStatus{
				State: api.NodeStatus_READY,
			},
		},
	}

	initialTaskSet := []*api.Task{
		{
			ID:           "task1",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name1",
			},

			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
		},
		{
			ID:           "task2",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name2",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
			NodeID: initialNodeSet[0].ID,
		},
		{
			ID:           "task3",
			DesiredState: api.TaskStateRunning,
			ServiceAnnotations: api.Annotations{
				Name: "name2",
			},
			Status: api.TaskStatus{
				State: api.TaskStateAllocated,
			},
			NodeID: initialNodeSet[0].ID,
		},
	}

	s := store.NewMemoryStore(nil)
	assert.NotNil(t, s)
	defer s.Close()

	err := s.Update(func(tx store.Tx) error {
		// Prepoulate nodes
		for _, n := range initialNodeSet {
			assert.NoError(t, store.CreateNode(tx, n))
		}

		// Prepopulate tasks
		for _, task := range initialTaskSet {
			assert.NoError(t, store.CreateTask(tx, task))
		}
		return nil
	})
	assert.NoError(t, err)

	scheduler := New(s)

	watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})
	defer cancel()

	go func() {
		assert.NoError(t, scheduler.Run(ctx))
	}()

	//preassigned tasks would be processed first
	assignment1 := watchAssignment(t, watch)
	// task2 and task3 are preassigned to node1
	assert.Equal(t, assignment1.NodeID, "node1")
	assert.Regexp(t, assignment1.ID, "(task2|task3)")

	assignment2 := watchAssignment(t, watch)
	if assignment1.ID == "task2" {
		assert.Equal(t, "task3", assignment2.ID)
	} else {
		assert.Equal(t, "task2", assignment2.ID)
	}

	// task1 would be assigned to node2 because node1 has 2 tasks already
	assignment3 := watchAssignment(t, watch)
	assert.Equal(t, assignment3.ID, "task1")
	assert.Equal(t, assignment3.NodeID, "node2")
}

func watchAssignment(t *testing.T, watch chan events.Event) *api.Task {
	for {
		select {
		case event := <-watch:
			if task, ok := event.(state.EventUpdateTask); ok {
				if task.Task.Status.State <= api.TaskStateRunning && task.Task.NodeID != "" {
					return task.Task
				}
			}
		case <-time.After(time.Second):
			t.Fatalf("no task assignment")
		}
	}
}

func TestSchedulerPluginConstraint(t *testing.T) {
	t.Skip("plugin filtering disabled since plugins on the engine are lazy loaded")
	ctx := context.Background()

	// Node1: vol plugin1
	n1 := &api.Node{
		ID: "node1_ID",
		Spec: api.NodeSpec{
			Annotations: api.Annotations{
				Name: "node1",
			},
		},
		Description: &api.NodeDescription{
			Engine: &api.EngineDescription{
				Plugins: []api.PluginDescription{
					{
						Type: "Volume",
						Name: "plugin1",
					},
				},
			},
		},
		Status: api.NodeStatus{
			State: api.NodeStatus_READY,
		},
	}

	// Node2: vol plugin1, vol plugin2
	n2 := &api.Node{
		ID: "node2_ID",
		Spec: api.NodeSpec{
			Annotations: api.Annotations{
				Name: "node2",
			},
		},
		Description: &api.NodeDescription{
			Engine: &api.EngineDescription{
				Plugins: []api.PluginDescription{
					{
						Type: "Volume",
						Name: "plugin1",
					},
					{
						Type: "Volume",
						Name: "plugin2",
					},
				},
			},
		},
		Status: api.NodeStatus{
			State: api.NodeStatus_READY,
		},
	}

	// Node3: vol plugin1, network plugin1
	n3 := &api.Node{
		ID: "node3_ID",
		Spec: api.NodeSpec{
			Annotations: api.Annotations{
				Name: "node3",
			},
		},
		Description: &api.NodeDescription{
			Engine: &api.EngineDescription{
				Plugins: []api.PluginDescription{
					{
						Type: "Volume",
						Name: "plugin1",
					},
					{
						Type: "Network",
						Name: "plugin1",
					},
				},
			},
		},
		Status: api.NodeStatus{
			State: api.NodeStatus_READY,
		},
	}

	volumeOptionsDriver := func(driver string) *api.Mount_VolumeOptions {
		return &api.Mount_VolumeOptions{
			DriverConfig: &api.Driver{
				Name: driver,
			},
		}
	}

	// Task1: vol plugin1
	t1 := &api.Task{
		ID:           "task1_ID",
		DesiredState: api.TaskStateRunning,
		Spec: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Mounts: []api.Mount{
						{
							Source:        "testVol1",
							Target:        "/foo",
							Type:          api.MountTypeVolume,
							VolumeOptions: volumeOptionsDriver("plugin1"),
						},
					},
				},
			},
		},
		ServiceAnnotations: api.Annotations{
			Name: "task1",
		},
		Status: api.TaskStatus{
			State: api.TaskStateAllocated,
		},
	}

	// Task2: vol plugin1, vol plugin2
	t2 := &api.Task{
		ID:           "task2_ID",
		DesiredState: api.TaskStateRunning,
		Spec: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Mounts: []api.Mount{
						{
							Source:        "testVol1",
							Target:        "/foo",
							Type:          api.MountTypeVolume,
							VolumeOptions: volumeOptionsDriver("plugin1"),
						},
						{
							Source:        "testVol2",
							Target:        "/foo",
							Type:          api.MountTypeVolume,
							VolumeOptions: volumeOptionsDriver("plugin2"),
						},
					},
				},
			},
		},
		ServiceAnnotations: api.Annotations{
			Name: "task2",
		},
		Status: api.TaskStatus{
			State: api.TaskStateAllocated,
		},
	}

	// Task3: vol plugin1, network plugin1
	t3 := &api.Task{
		ID:           "task3_ID",
		DesiredState: api.TaskStateRunning,
		Networks: []*api.NetworkAttachment{
			{
				Network: &api.Network{
					ID: "testNwID1",
					Spec: api.NetworkSpec{
						Annotations: api.Annotations{
							Name: "testVol1",
						},
					},
					DriverState: &api.Driver{
						Name: "plugin1",
					},
				},
			},
		},
		Spec: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Mounts: []api.Mount{
						{
							Source:        "testVol1",
							Target:        "/foo",
							Type:          api.MountTypeVolume,
							VolumeOptions: volumeOptionsDriver("plugin1"),
						},
					},
				},
			},
		},
		ServiceAnnotations: api.Annotations{
			Name: "task2",
		},
		Status: api.TaskStatus{
			State: api.TaskStateAllocated,
		},
	}

	s := store.NewMemoryStore(nil)
	assert.NotNil(t, s)
	defer s.Close()

	// Add initial node and task
	err := s.Update(func(tx store.Tx) error {
		assert.NoError(t, store.CreateTask(tx, t1))
		assert.NoError(t, store.CreateNode(tx, n1))
		return nil
	})
	assert.NoError(t, err)

	scheduler := New(s)

	watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})
	defer cancel()

	go func() {
		assert.NoError(t, scheduler.Run(ctx))
	}()
	defer scheduler.Stop()

	// t1 should get assigned
	assignment := watchAssignment(t, watch)
	assert.Equal(t, assignment.NodeID, "node1_ID")

	// Create t2; it should stay in the pending state because there is
	// no node that with volume plugin `plugin2`
	err = s.Update(func(tx store.Tx) error {
		assert.NoError(t, store.CreateTask(tx, t2))
		return nil
	})
	assert.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	s.View(func(tx store.ReadTx) {
		task := store.GetTask(tx, "task2_ID")
		if task.Status.State >= api.TaskStateAssigned {
			t.Fatalf("task 'task2_ID' should not have been assigned to node %v", task.NodeID)
		}
	})

	// Now add the second node
	err = s.Update(func(tx store.Tx) error {
		assert.NoError(t, store.CreateNode(tx, n2))
		return nil
	})
	assert.NoError(t, err)

	// Check that t2 has been assigned
	assignment1 := watchAssignment(t, watch)
	assert.Equal(t, assignment1.ID, "task2_ID")
	assert.Equal(t, assignment1.NodeID, "node2_ID")

	// Create t3; it should stay in the pending state because there is
	// no node that with network plugin `plugin1`
	err = s.Update(func(tx store.Tx) error {
		assert.NoError(t, store.CreateTask(tx, t3))
		return nil
	})
	assert.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	s.View(func(tx store.ReadTx) {
		task := store.GetTask(tx, "task3_ID")
		if task.Status.State >= api.TaskStateAssigned {
			t.Fatal("task 'task3_ID' should not have been assigned")
		}
	})

	// Now add the node3
	err = s.Update(func(tx store.Tx) error {
		assert.NoError(t, store.CreateNode(tx, n3))
		return nil
	})
	assert.NoError(t, err)

	// Check that t3 has been assigned
	assignment2 := watchAssignment(t, watch)
	assert.Equal(t, assignment2.ID, "task3_ID")
	assert.Equal(t, assignment2.NodeID, "node3_ID")
}

func BenchmarkScheduler1kNodes1kTasks(b *testing.B) {
	benchScheduler(b, 1e3, 1e3, false)
}

func BenchmarkScheduler1kNodes10kTasks(b *testing.B) {
	benchScheduler(b, 1e3, 1e4, false)
}

func BenchmarkScheduler1kNodes100kTasks(b *testing.B) {
	benchScheduler(b, 1e3, 1e5, false)
}

func BenchmarkScheduler100kNodes100kTasks(b *testing.B) {
	benchScheduler(b, 1e5, 1e5, false)
}

func BenchmarkScheduler100kNodes1kTasks(b *testing.B) {
	benchScheduler(b, 1e5, 1e3, false)
}

func BenchmarkScheduler100kNodes1MTasks(b *testing.B) {
	benchScheduler(b, 1e5, 1e6, false)
}

func BenchmarkSchedulerConstraints1kNodes1kTasks(b *testing.B) {
	benchScheduler(b, 1e3, 1e3, true)
}

func BenchmarkSchedulerConstraints1kNodes10kTasks(b *testing.B) {
	benchScheduler(b, 1e3, 1e4, true)
}

func BenchmarkSchedulerConstraints1kNodes100kTasks(b *testing.B) {
	benchScheduler(b, 1e3, 1e5, true)
}

func BenchmarkSchedulerConstraints5kNodes100kTasks(b *testing.B) {
	benchScheduler(b, 5e3, 1e5, true)
}

func benchScheduler(b *testing.B, nodes, tasks int, networkConstraints bool) {
	ctx := context.Background()

	for iters := 0; iters < b.N; iters++ {
		b.StopTimer()
		s := store.NewMemoryStore(nil)
		scheduler := New(s)

		watch, cancel := state.Watch(s.WatchQueue(), state.EventUpdateTask{})

		go func() {
			_ = scheduler.Run(ctx)
		}()

		// Let the scheduler get started
		runtime.Gosched()

		_ = s.Update(func(tx store.Tx) error {
			// Create initial nodes and tasks
			for i := 0; i < nodes; i++ {
				n := &api.Node{
					ID: identity.NewID(),
					Spec: api.NodeSpec{
						Annotations: api.Annotations{
							Name:   "name" + strconv.Itoa(i),
							Labels: make(map[string]string),
						},
					},
					Status: api.NodeStatus{
						State: api.NodeStatus_READY,
					},
					Description: &api.NodeDescription{
						Engine: &api.EngineDescription{},
					},
				}
				// Give every third node a special network
				if i%3 == 0 {
					n.Description.Engine.Plugins = []api.PluginDescription{
						{
							Name: "network",
							Type: "Network",
						},
					}

				}
				err := store.CreateNode(tx, n)
				if err != nil {
					panic(err)
				}
			}
			for i := 0; i < tasks; i++ {
				id := "task" + strconv.Itoa(i)
				t := &api.Task{
					ID:           id,
					DesiredState: api.TaskStateRunning,
					ServiceAnnotations: api.Annotations{
						Name: id,
					},
					Status: api.TaskStatus{
						State: api.TaskStateAllocated,
					},
				}
				if networkConstraints {
					t.Networks = []*api.NetworkAttachment{
						{
							Network: &api.Network{
								DriverState: &api.Driver{
									Name: "network",
								},
							},
						},
					}
				}
				err := store.CreateTask(tx, t)
				if err != nil {
					panic(err)
				}
			}
			b.StartTimer()
			return nil
		})

		for i := 0; i != tasks; i++ {
			<-watch
		}

		scheduler.Stop()
		cancel()
		s.Close()
	}
}
