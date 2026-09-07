package fat

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

// TestClusterAllocationBasics tests the fundamental cluster allocation behavior
func TestClusterAllocationBasics(t *testing.T) {
	// Create a small FAT32 filesystem for testing
	const size = 1024 * 1024 * 1024 // 1GB - forces FAT32
	vmFS := vm.NewVirtualMemory(size, 4096)
	writer, err := CreateFATFileSystem(vmFS, size)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}

	// Test basic cluster allocation
	t.Run("BasicAllocation", func(t *testing.T) {
		initialFree := writer.fatTable.GetFreeClusterCount()

		// Allocate a cluster
		cluster1, err := writer.fatTable.AllocateCluster()
		if err != nil {
			t.Fatalf("Failed to allocate cluster: %v", err)
		}

		if cluster1 < 2 {
			t.Errorf("Allocated cluster %d should be >= 2", cluster1)
		}

		// Check that cluster is marked as allocated
		if !writer.fatTable.IsAllocated(cluster1) {
			t.Errorf("Cluster %d should be marked as allocated", cluster1)
		}

		// Check that free count decreased
		newFree := writer.fatTable.GetFreeClusterCount()
		if newFree != initialFree-1 {
			t.Errorf("Free cluster count should decrease by 1: expected %d, got %d", initialFree-1, newFree)
		}
	})

	// Test double allocation prevention
	t.Run("NoDoubleAllocation", func(t *testing.T) {
		allocatedClusters := make(map[uint32]bool)

		// Allocate 10 clusters and ensure no duplicates
		for i := 0; i < 10; i++ {
			cluster, err := writer.fatTable.AllocateCluster()
			if err != nil {
				t.Fatalf("Failed to allocate cluster %d: %v", i, err)
			}

			if allocatedClusters[cluster] {
				t.Errorf("Cluster %d was allocated twice!", cluster)
			}
			allocatedClusters[cluster] = true

			if !writer.fatTable.IsAllocated(cluster) {
				t.Errorf("Cluster %d should be marked as allocated", cluster)
			}
		}
	})
}

// TestClusterChainBuilding tests that cluster chains are built correctly
func TestClusterChainBuilding(t *testing.T) {
	const size = 1024 * 1024 * 1024 // 1GB - forces FAT32
	vmFS := vm.NewVirtualMemory(size, 4096)
	writer, err := CreateFATFileSystem(vmFS, size)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}

	t.Run("SimpleChain", func(t *testing.T) {
		// Allocate 3 clusters
		clusters := make([]uint32, 3)
		for i := range clusters {
			cluster, err := writer.fatTable.AllocateCluster()
			if err != nil {
				t.Fatalf("Failed to allocate cluster %d: %v", i, err)
			}
			clusters[i] = cluster
		}

		// Build chain: cluster[0] -> cluster[1] -> cluster[2] -> EOC
		for i := 0; i < len(clusters)-1; i++ {
			err := writer.fatTable.SetEntry(clusters[i], clusters[i+1])
			if err != nil {
				t.Fatalf("Failed to set chain link %d->%d: %v", clusters[i], clusters[i+1], err)
			}
		}

		// Mark last cluster as EOC
		err = writer.fatTable.MarkEndOfChain(clusters[len(clusters)-1])
		if err != nil {
			t.Fatalf("Failed to mark cluster %d as EOC: %v", clusters[len(clusters)-1], err)
		}

		// Verify chain
		chain := writer.fatTable.GetClusterChain(clusters[0])

		if len(chain) != len(clusters) {
			t.Errorf("Chain length mismatch: expected %d, got %d", len(clusters), len(chain))
		}

		for i, cluster := range clusters {
			if i >= len(chain) || chain[i] != cluster {
				t.Errorf("Chain mismatch at position %d: expected %d, got %d", i, cluster, chain[i])
			}
		}

		// Verify that the last cluster is marked as EOC
		lastValue := writer.fatTable.GetEntry(clusters[len(clusters)-1])
		if !writer.fatTable.IsEndOfChain(lastValue) {
			t.Errorf("Last cluster %d should be marked as EOC, got value %d", clusters[len(clusters)-1], lastValue)
		}
	})
}

// TestFileAllocation tests the complete file allocation process
func TestFileAllocation(t *testing.T) {
	const size = 1024 * 1024 * 1024 // 1GB - forces FAT32
	vmFS := vm.NewVirtualMemory(size, 4096)
	writer, err := CreateFATFileSystem(vmFS, size)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}

	t.Run("SingleFileAllocation", func(t *testing.T) {
		initialFree := writer.fatTable.GetFreeClusterCount()

		// Create a test file
		node, err := writer.AllocateNode()
		if err != nil {
			t.Fatalf("Failed to allocate node: %v", err)
		}

		err = node.SetName("test.txt")
		if err != nil {
			t.Fatalf("Failed to set node name: %v", err)
		}

		// Create test content (2 clusters worth to test chain building)
		clusterSize := int64(writer.layout.ClusterSize())
		contentSize := clusterSize + clusterSize/2 // 1.5 clusters
		content := make([]byte, contentSize)
		for i := range content {
			content[i] = byte(i % 256)
		}
		contentRegion := vm.RawRegion(content)

		// Write content
		err = writer.WriteContents(node, &contentRegion)
		if err != nil {
			t.Fatalf("Failed to write contents: %v", err)
		}

		// Verify that clusters were allocated (should be 2 clusters for 1.5 clusters of data)
		expectedClustersUsed := uint32(2)
		actualFree := writer.fatTable.GetFreeClusterCount()
		actualUsed := initialFree - actualFree

		if actualUsed != expectedClustersUsed {
			t.Errorf("Expected %d clusters to be used, but %d were used", expectedClustersUsed, actualUsed)
		}

		// Verify no clusters are in "allocated" state (they should all be in chains)
		allocatedCount := writer.fatTable.GetAllocatedClusterCount()
		if allocatedCount != 0 {
			t.Errorf("Expected 0 clusters in allocated state, but found %d", allocatedCount)
		}
	})
}

// TestFSInfoCalculation tests that FSInfo sector reflects correct free cluster counts
func TestFSInfoCalculation(t *testing.T) {
	const size = 1024 * 1024 * 1024 // 1GB - forces FAT32
	vmFS := vm.NewVirtualMemory(size, 4096)
	writer, err := CreateFATFileSystem(vmFS, size)
	if err != nil {
		t.Fatalf("Failed to create FAT filesystem: %v", err)
	}

	t.Run("FSInfoAfterFileCreation", func(t *testing.T) {
		// Create some files to allocate clusters
		var files []interface{} // Would be []fs.WritableNode in real usage
		for i := 0; i < 3; i++ {
			node, err := writer.AllocateNode()
			if err != nil {
				t.Fatalf("Failed to allocate node %d: %v", i, err)
			}

			err = node.SetName("file" + string(rune('0'+i)) + ".txt")
			if err != nil {
				t.Fatalf("Failed to set name for node %d: %v", i, err)
			}

			// Small content (1 cluster)
			content := make([]byte, 1024)
			for j := range content {
				content[j] = byte(j % 256)
			}
			contentRegion := vm.RawRegion(content)

			err = writer.WriteContents(node, &contentRegion)
			if err != nil {
				t.Fatalf("Failed to write contents for node %d: %v", i, err)
			}

			files = append(files, node)
		}

		// Get root directory and set up directory structure
		root, err := writer.WritableRootDirectory()
		if err != nil {
			t.Fatalf("Failed to get root directory: %v", err)
		}

		// Write directory structure (this is required for proper finalization)
		// Note: In real usage, this would use the actual fs.WritableNode interface
		// For this test, we'll just call Finalize directly since our files are already allocated

		// Finalize to trigger FSInfo calculation
		err = writer.Finalize()
		if err != nil {
			t.Fatalf("Failed to finalize filesystem: %v", err)
		}

		// Use the variables to avoid compilation errors
		_ = files
		_ = root

		// Check that FSInfo reflects actual free cluster count
		expectedFree := writer.fatTable.GetFreeClusterCount()

		// Read FSInfo sector to verify
		fsInfo := writer.layout.FsInfo()
		if fsInfo == nil {
			t.Fatal("FSInfo sector not accessible")
		}

		actualFree := fsInfo.LastFreeClusterCount()
		if actualFree != expectedFree {
			t.Errorf("FSInfo free cluster count mismatch: expected %d, got %d", expectedFree, actualFree)
		}

		// Verify FSInfo signatures are correct
		if fsInfo.LeadSignature() != 0x41615252 {
			t.Errorf("FSInfo lead signature wrong: expected 0x41615252, got 0x%08X", fsInfo.LeadSignature())
		}

		if fsInfo.Signature() != 0x61417272 {
			t.Errorf("FSInfo signature wrong: expected 0x61417272, got 0x%08X", fsInfo.Signature())
		}

		if fsInfo.TrailSignature() != 0xAA550000 {
			t.Errorf("FSInfo trail signature wrong: expected 0xAA550000, got 0x%08X", fsInfo.TrailSignature())
		}
	})
}

// TestClusterStateTransitions tests the state machine for cluster allocation
func TestClusterStateTransitions(t *testing.T) {
	fatTable := NewFATTableRegion("FAT32", 1000)

	t.Run("StateTransitions", func(t *testing.T) {
		// Initial state: cluster should be free
		cluster := uint32(10)
		if !fatTable.IsFree(cluster) {
			t.Errorf("Cluster %d should initially be free", cluster)
		}

		// Allocate cluster: should become allocated
		err := fatTable.SetEntry(cluster, fatTable.getAllocatedMarker())
		if err != nil {
			t.Fatalf("Failed to mark cluster as allocated: %v", err)
		}

		if !fatTable.IsAllocated(cluster) {
			t.Errorf("Cluster %d should be in allocated state", cluster)
		}
		if fatTable.IsFree(cluster) {
			t.Errorf("Cluster %d should not be free after allocation", cluster)
		}

		// Put in chain: should no longer be allocated
		nextCluster := uint32(11)
		err = fatTable.SetEntry(cluster, nextCluster)
		if err != nil {
			t.Fatalf("Failed to set cluster chain: %v", err)
		}

		if fatTable.IsAllocated(cluster) {
			t.Errorf("Cluster %d should not be allocated after being put in chain", cluster)
		}
		if fatTable.IsFree(cluster) {
			t.Errorf("Cluster %d should not be free after being put in chain", cluster)
		}

		// Mark as EOC: should be end of chain
		err = fatTable.MarkEndOfChain(nextCluster)
		if err != nil {
			t.Fatalf("Failed to mark cluster as EOC: %v", err)
		}

		if !fatTable.IsEndOfChain(fatTable.GetEntry(nextCluster)) {
			t.Errorf("Cluster %d should be marked as EOC", nextCluster)
		}

		// Free cluster: should become free again
		err = fatTable.MarkFree(cluster)
		if err != nil {
			t.Fatalf("Failed to mark cluster as free: %v", err)
		}

		if !fatTable.IsFree(cluster) {
			t.Errorf("Cluster %d should be free after being marked free", cluster)
		}
	})
}
