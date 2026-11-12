package conformance

import (
 "crypto/sha256"
 "fmt"
 "sort"
 "testing"
)
func TestProofExportDigest(t *testing.T) {
 for _, version := range []string{SchemaV2, SchemaV3} {
  suite, err := NewSuite(t.Context(), version, Reference{}.Declare()); if err != nil { t.Fatal(err) }
  files, err := suite.ExportFiles(t.Context()); if err != nil { t.Fatal(err) }
  names := make([]string, 0, len(files)); for name := range files { names = append(names, name) }; sort.Strings(names)
  for _, name := range names { fmt.Printf("DIGEST %s %s %x\n", version, name, sha256.Sum256(files[name])) }
 }
}
