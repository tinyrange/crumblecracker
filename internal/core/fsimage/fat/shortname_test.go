package fat

import (
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

// TestShortNameGeneration tests various short name generation scenarios
func TestShortNameGeneration(t *testing.T) {
	// Create a test writer
	vmFS := vm.NewVirtualMemory(1024*1024, 4096)
	writer, err := CreateFATFileSystem(vmFS, 1024*1024)
	if err != nil {
		t.Fatalf("Failed to create test filesystem: %v", err)
	}

	tests := []struct {
		name         string
		longName     string
		isDir        bool
		expectedName string
		expectedExt  string
		description  string
	}{
		{
			name:         "Simple8.3",
			longName:     "test.txt",
			isDir:        false,
			expectedName: "TEST    ",
			expectedExt:  "TXT",
			description:  "Simple 8.3 compliant name",
		},
		{
			name:         "LongBaseName",
			longName:     "verylongfilename.txt",
			isDir:        false,
			expectedName: "VERYLONG",
			expectedExt:  "TXT",
			description:  "Long base name should be truncated",
		},
		{
			name:         "LongExtension",
			longName:     "test.longext",
			isDir:        false,
			expectedName: "TEST    ",
			expectedExt:  "LON",
			description:  "Long extension should be truncated",
		},
		{
			name:         "InvalidChars",
			longName:     "file with spaces.txt",
			isDir:        false,
			expectedName: "FILE_WIT",
			expectedExt:  "TXT",
			description:  "Spaces should be replaced with underscores",
		},
		{
			name:         "SpecialChars",
			longName:     "file+name*.txt",
			isDir:        false,
			expectedName: "FILE_NAM",
			expectedExt:  "TXT",
			description:  "Special characters should be replaced",
		},
		{
			name:         "DotEntry",
			longName:     ".",
			isDir:        true,
			expectedName: ".       ",
			expectedExt:  "   ",
			description:  "Dot directory entry",
		},
		{
			name:         "DotDotEntry",
			longName:     "..",
			isDir:        true,
			expectedName: "..      ",
			expectedExt:  "   ",
			description:  "Dot-dot directory entry",
		},
		{
			name:         "NoExtension",
			longName:     "filename",
			isDir:        false,
			expectedName: "FILENAME",
			expectedExt:  "   ",
			description:  "File with no extension",
		},
		{
			name:         "MultipleDots",
			longName:     "file.name.txt",
			isDir:        false,
			expectedName: "FILE_N~1",
			expectedExt:  "TXT",
			description:  "Multiple dots - should use last as extension separator with collision tail",
		},
		{
			name:         "LeadingDots",
			longName:     "...file.txt",
			isDir:        false,
			expectedName: "FILE    ",
			expectedExt:  "TXT",
			description:  "Leading dots should be trimmed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortName := writer.generateShortName(tt.longName, tt.isDir)

			actualName := string(shortName.Name[:])
			actualExt := string(shortName.Extension[:])

			if actualName != tt.expectedName {
				t.Errorf("Name mismatch for %s: expected '%s', got '%s'",
					tt.description, tt.expectedName, actualName)
			}

			if actualExt != tt.expectedExt {
				t.Errorf("Extension mismatch for %s: expected '%s', got '%s'",
					tt.description, tt.expectedExt, actualExt)
			}
		})
	}
}

// TestShortNameCollisionResolution tests collision handling
func TestShortNameCollisionResolution(t *testing.T) {
	vmFS := vm.NewVirtualMemory(1024*1024, 4096)
	writer, err := CreateFATFileSystem(vmFS, 1024*1024)
	if err != nil {
		t.Fatalf("Failed to create test filesystem: %v", err)
	}

	// Test collision resolution by generating the exact same name twice
	baseName := "verylongfilename.txt"

	// Generate first name - should be truncated without tail
	first := writer.generateShortName(baseName, false)
	expectedFirst := "VERYLONG"
	actualFirst := string(first.Name[:])

	if string(first.Name[:8]) != expectedFirst {
		t.Errorf("First name should be '%s', got '%s'", expectedFirst, actualFirst[:8])
	}

	// Generate the exact same name again - should get numeric tail due to collision
	second := writer.generateShortName(baseName, false)
	actualSecond := string(second.Name[:])

	// The second name should be different from the first due to collision
	if actualFirst == actualSecond {
		t.Errorf("Second name should be different from first due to collision detection")
		t.Logf("First: '%s', Second: '%s'", actualFirst, actualSecond)
	}

	if string(second.Name[:8]) != "VERYLO~1" {
		t.Errorf("Second name should use numeric tail VERYLO~1, got '%s'", actualSecond[:8])
	}
}

// TestValid83Name tests the 8.3 name validation
func TestValid83Name(t *testing.T) {
	vmFS := vm.NewVirtualMemory(1024*1024, 4096)
	writer, err := CreateFATFileSystem(vmFS, 1024*1024)
	if err != nil {
		t.Fatalf("Failed to create test filesystem: %v", err)
	}

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"ValidShort", "TEST.TXT", true},
		{"ValidNoExt", "FILENAME", true},
		{"TooLongBase", "VERYLONGNAME.TXT", false},
		{"TooLongExt", "TEST.LONGEXT", false},
		{"HasSpaces", "FILE NAME.TXT", false},
		{"HasInvalidChars", "FILE*.TXT", false},
		{"LowerCase", "test.txt", false},
		{"MultipleDots", "FILE.NAME.TXT", false},
		{"EmptyName", "", false},
		{"JustDot", ".", false}, // Special case - not valid for regular files
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := writer.isValid83Name(tt.input)
			if result != tt.expected {
				t.Errorf("isValid83Name('%s') = %v, expected %v", tt.input, result, tt.expected)
			}
		})
	}
}

// TestShortNameStringConversion tests conversion between ShortName and string
func TestShortNameStringConversion(t *testing.T) {
	vmFS := vm.NewVirtualMemory(1024*1024, 4096)
	writer, err := CreateFATFileSystem(vmFS, 1024*1024)
	if err != nil {
		t.Fatalf("Failed to create test filesystem: %v", err)
	}

	tests := []struct {
		name      string
		shortName ShortName
		expected  string
	}{
		{
			name: "WithExtension",
			shortName: ShortName{
				Name:      [8]byte{'T', 'E', 'S', 'T', ' ', ' ', ' ', ' '},
				Extension: [3]byte{'T', 'X', 'T'},
			},
			expected: "TEST.TXT",
		},
		{
			name: "NoExtension",
			shortName: ShortName{
				Name:      [8]byte{'F', 'I', 'L', 'E', 'N', 'A', 'M', 'E'},
				Extension: [3]byte{' ', ' ', ' '},
			},
			expected: "FILENAME",
		},
		{
			name: "DotEntry",
			shortName: ShortName{
				Name:      [8]byte{'.', ' ', ' ', ' ', ' ', ' ', ' ', ' '},
				Extension: [3]byte{' ', ' ', ' '},
			},
			expected: ".",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := writer.shortNameToString(tt.shortName)
			if result != tt.expected {
				t.Errorf("shortNameToString() = '%s', expected '%s'", result, tt.expected)
			}
		})
	}
}
