package rewind

import (
	"errors"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"
)

type readThroughStorage struct {
	storage.Storer
	secondary storer.EncodedObjectStorer
}

func (s *readThroughStorage) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := s.Storer.EncodedObject(t, h)
	if err == nil {
		return obj, nil
	}
	if errors.Is(err, plumbing.ErrObjectNotFound) && s.secondary != nil {
		return s.secondary.EncodedObject(t, h)
	}
	return obj, err
}

func (s *readThroughStorage) HasEncodedObject(h plumbing.Hash) error {
	if err := s.Storer.HasEncodedObject(h); err == nil {
		return nil
	} else if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return err
	}
	if s.secondary != nil {
		return s.secondary.HasEncodedObject(h)
	}
	return plumbing.ErrObjectNotFound
}

func (s *readThroughStorage) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	size, err := s.Storer.EncodedObjectSize(h)
	if err == nil {
		return size, nil
	}
	if errors.Is(err, plumbing.ErrObjectNotFound) && s.secondary != nil {
		return s.secondary.EncodedObjectSize(h)
	}
	return size, err
}
