package sori

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

func NewCollectionManager(rootDir string, initial ...VolumeEntry) (*CollectionManager, error) {
	coll, err := LoadOrNewCollection(rootDir, initial...)
	if err != nil {
		return nil, err
	}

	m := &CollectionManager{
		root:  rootDir,
		coll:  coll,
		byRef: make(map[string]int, len(coll.Volumes)),
	}
	for i, entry := range coll.Volumes {
		ref := entry.Index.VolumeRef
		if ref == "" {
			continue
		}
		if _, dup := m.byRef[ref]; dup {
			return nil, validationError("NewCollectionManager",
				fmt.Sprintf("duplicate VolumeRef %q in persisted collection", ref), nil)
		}
		m.byRef[ref] = i
	}
	return m, nil
}

func LoadOrNewCollection(rootDir string, initialEntries ...VolumeEntry) (*VolumeCollection, error) {
	path := filepath.Join(rootDir, CollectionJson)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			coll := NewVolumeCollection(initialEntries...)
			if err := saveCollection(rootDir, *coll); err != nil {
				return nil, transportError("LoadOrNewCollection", "save new collection", err)
			}
			return coll, nil
		}
		return nil, transportError("LoadOrNewCollection", "read collection file", err)
	}

	var coll VolumeCollection
	if err := json.Unmarshal(data, &coll); err != nil {
		return nil, validationError("LoadOrNewCollection", "unmarshal collection JSON", err)
	}
	return &coll, nil
}

func NewVolumeCollection(initialEntries ...VolumeEntry) *VolumeCollection {
	coll := &VolumeCollection{
		Version: 1,
		Volumes: make([]VolumeEntry, len(initialEntries)),
	}
	copy(coll.Volumes, initialEntries)
	return coll
}

func (m *CollectionManager) AddOrUpdate(v VolumeEntry) error {
	if v.Index.VolumeRef == "" {
		return validationError("CollectionManager.AddOrUpdate", "VolumeRef must not be empty", nil)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	ref := v.Index.VolumeRef
	if idx, ok := m.byRef[ref]; ok {
		if !reflect.DeepEqual(m.coll.Volumes[idx], v) {
			m.coll.Volumes[idx] = v
			m.coll.Version++
		} else {
			return nil
		}
	} else {
		m.coll.Volumes = append(m.coll.Volumes, v)
		m.byRef[ref] = len(m.coll.Volumes) - 1
		m.coll.Version++
	}

	return saveCollection(m.root, *m.coll)
}

func (m *CollectionManager) Remove(ref string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx, ok := m.byRef[ref]
	if !ok {
		return false, nil
	}

	last := len(m.coll.Volumes) - 1
	if idx != last {
		m.coll.Volumes[idx] = m.coll.Volumes[last]
		movedRef := m.coll.Volumes[idx].Index.VolumeRef
		m.byRef[movedRef] = idx
	}

	m.coll.Volumes = m.coll.Volumes[:last]
	delete(m.byRef, ref)
	m.coll.Version++

	return true, saveCollection(m.root, *m.coll)
}

func (m *CollectionManager) GetSnapshot() VolumeCollection {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := VolumeCollection{
		Version: m.coll.Version,
		Volumes: make([]VolumeEntry, len(m.coll.Volumes)),
	}
	for i, entry := range m.coll.Volumes {
		blobCopy := make(ConfigBlob, len(entry.ConfigBlob))
		for k, v := range entry.ConfigBlob {
			blobCopy[k] = v
		}
		out.Volumes[i] = VolumeEntry{
			Index: VolumeIndex{
				VolumeRef:   entry.Index.VolumeRef,
				DisplayName: entry.Index.DisplayName,
				CreatedAt:   entry.Index.CreatedAt,
				Partitions:  append([]Partition(nil), entry.Index.Partitions...),
			},
			ConfigBlob: blobCopy,
		}
	}
	return out
}

func (m *CollectionManager) Get(ref string) (VolumeEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	idx, ok := m.byRef[ref]
	if !ok {
		return VolumeEntry{}, false
	}
	entry := m.coll.Volumes[idx]
	cb := make(ConfigBlob, len(entry.ConfigBlob))
	for k, v := range entry.ConfigBlob {
		cb[k] = v
	}
	return VolumeEntry{
		Index: VolumeIndex{
			VolumeRef:   entry.Index.VolumeRef,
			DisplayName: entry.Index.DisplayName,
			CreatedAt:   entry.Index.CreatedAt,
			Partitions:  append([]Partition(nil), entry.Index.Partitions...),
		},
		ConfigBlob: cb,
	}, true
}

func (m *CollectionManager) Flush() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return saveCollection(m.root, *m.coll)
}

func (m *CollectionManager) PublishVolumeFromDir(ctx context.Context, volDir, displayName, tag string) error {
	rawConfig, err := ValidateVolumeDir(volDir)
	if err != nil {
		return err
	}

	var cb ConfigBlob
	if err := json.Unmarshal(rawConfig, &cb); err != nil {
		return validationError("CollectionManager.PublishVolumeFromDir", "parse configblob.json", err)
	}

	vi, err := GenerateVolumeIndex(volDir, displayName)
	if err != nil {
		return err
	}

	client := NewClient(WithLocalStorePath(m.root))
	newVi, err := client.PublishVolume(ctx, vi, volDir, tag, rawConfig)
	if err != nil {
		return err
	}
	if newVi == nil {
		return integrityError("CollectionManager.PublishVolumeFromDir", "publish returned nil VolumeIndex", nil)
	}

	entry := VolumeEntry{
		Index:      *newVi,
		ConfigBlob: cb,
	}
	if err := m.AddOrUpdate(entry); err != nil {
		return err
	}

	return nil
}

func (vc *VolumeCollection) HasVolume(vi VolumeIndex) bool {
	for _, entry := range vc.Volumes {
		if entry.Index.DisplayName == vi.DisplayName ||
			entry.Index.VolumeRef == vi.VolumeRef {
			return true
		}
	}
	return false
}

func (vc *VolumeCollection) Merge(newColl VolumeCollection) bool {
	added := false
	for _, entry := range newColl.Volumes {
		if !vc.HasVolume(entry.Index) {
			vc.Volumes = append(vc.Volumes, entry)
			added = true
		}
	}
	if added {
		vc.Version++
	}
	return added
}

func (vc *VolumeCollection) AddVolume(entry VolumeEntry) {
	vc.Volumes = append(vc.Volumes, entry)
	vc.Version++
}

func (vc *VolumeCollection) RemoveVolume(idx int) error {
	if idx < 0 || idx >= len(vc.Volumes) {
		return validationError("VolumeCollection.RemoveVolume", fmt.Sprintf("index %d out of range", idx), nil)
	}
	vc.Volumes = append(vc.Volumes[:idx], vc.Volumes[idx+1:]...)
	vc.Version++
	return nil
}

func saveCollection(rootDir string, coll VolumeCollection) error {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return transportError("saveCollection", fmt.Sprintf("create collection dir %q", rootDir), err)
	}
	path := filepath.Join(rootDir, CollectionJson)
	data, err := json.MarshalIndent(coll, "", "  ")
	if err != nil {
		return transportError("saveCollection", "marshal collection", err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return transportError("saveCollection", fmt.Sprintf("write collection file %q", path), err)
	}
	return nil
}
