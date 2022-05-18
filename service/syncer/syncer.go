// Copyright 2019 Drone IO, Inc.
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

package syncer

import (
	"context"
	"runtime/debug"
	"strings"
	"time"

	"github.com/drone/drone/core"

	"github.com/sirupsen/logrus"
)

// New returns a new Synchronizer.
func New(
	repoz core.RepositoryService,
	repos core.RepositoryStore,
	users core.UserStore,
	batch core.Batcher,
) *Synchronizer {
	return &Synchronizer{
		repoz: repoz,
		repos: repos,
		users: users,
		batch: batch,
		match: noopFilter,
	}
}

// Synchronizer synchronizes user repositories and permissions
// between a remote source code management system and the local
// data store.
type Synchronizer struct {
	repoz core.RepositoryService
	repos core.RepositoryStore
	users core.UserStore
	batch core.Batcher
	match FilterFunc
}

// SetFilter sets the filter function.
func (s *Synchronizer) SetFilter(fn FilterFunc) {
	s.match = fn
}

func (s *Synchronizer) SyncSingle(ctx context.Context, user *core.User, namespace string, name string) error {
	repoSlug := namespace + "/" + name

	logger := logrus.WithField("login", user.Login)
	logger.Debugln("syncer: begin sync single repository " + repoSlug)

	defer func() {
		// taking the paranoid approach to recover from
		// a panic that should absolutely never happen.
		if err := recover(); err != nil {
			logger = logger.WithField("error", err)
			logger.Errorf("syncer: unexpected panic\n%s\n", debug.Stack())
		}
	}()

	remote, err := s.repoz.Find(ctx, user, repoSlug)
	if err != nil {
		logger = logger.WithError(err)
		logger.Warnln("syncer: cannot get repository")
		return err
	}
	local, err := s.repos.FindName(ctx, namespace, name)

	// There may be other errors that could lead to us not finding it, but let's ignore and just assume that it doesn't exist
	if err != nil {
		remote.Synced = time.Now().Unix()
		remote.Created = time.Now().Unix()
		remote.Updated = time.Now().Unix()
		remote.Version = 1
		err := s.repos.Create(ctx, remote)
		if err != nil {
			logger = logger.WithError(err)
			logger.Warnln("syncer: cannot create")
			return err
		}
	} else if diff(local, remote) {
		merge(local, remote)
		local.Synced = time.Now().Unix()
		local.Updated = time.Now().Unix()
		err := s.repos.Update(ctx, local)
		if err != nil {
			logger = logger.WithError(err)
			logger.Warnln("syncer: cannot update")
			return err
		}
	}

	logger.Debugln("syncer: finished sync single repository " + repoSlug)
	return nil
}

// Sync synchronizes the user repository list in 6 easy steps.
func (s *Synchronizer) Sync(ctx context.Context, user *core.User) (*core.Batch, error) {
	logger := logrus.WithField("login", user.Login)
	logger.Debugln("syncer: begin repository sync")
	return nil, nil

	defer func() {
		// taking the paranoid approach to recover from
		// a panic that should absolutely never happen.
		if err := recover(); err != nil {
			logger = logger.WithField("error", err)
			logger.Errorf("syncer: unexpected panic\n%s\n", debug.Stack())
		}

		// when the synchronization process is complete
		// be sure to update the user sync date.
		user.Syncing = false
		user.Synced = time.Now().Unix()
		s.users.Update(context.Background(), user)
	}()

	if user.Syncing == false {
		user.Syncing = true
		err := s.users.Update(ctx, user)
		if err != nil {
			logger = logger.WithError(err)
			logger.Warnln("syncer: cannot update user")
			return nil, err
		}
	}

	batch := &core.Batch{}
	remote := map[string]*core.Repository{}
	local := map[string]*core.Repository{}

	//
	// STEP1: get the list of repositories from the remote
	// source code management system (e.g. GitHub).
	//

	{
		repos, err := s.repoz.List(ctx, user)
		if err != nil {
			logger = logger.WithError(err)
			logger.Warnln("syncer: cannot get remote repository list")
			return nil, err
		}
		for _, repo := range repos {
			if strings.Count(repo.Slug, "/") > 1 {
				if logrus.GetLevel() == logrus.TraceLevel {
					logger.WithField("namespace", repo.Namespace).
						WithField("name", repo.Name).
						WithField("uid", repo.UID).
						Traceln("syncer: skipping subrepositories")
				}
			} else if repo.Archived {
				if logrus.GetLevel() == logrus.TraceLevel {
					logger.WithField("namespace", repo.Namespace).
						WithField("name", repo.Name).
						WithField("uid", repo.UID).
						Traceln("syncer: skipping archived repositories")
				}
			} else if s.match(repo) {
				remote[repo.UID] = repo
				if logrus.GetLevel() == logrus.TraceLevel {
					logger.WithField("namespace", repo.Namespace).
						WithField("name", repo.Name).
						WithField("uid", repo.UID).
						Traceln("syncer: remote repository matches filter")
				}
			} else {
				if logrus.GetLevel() == logrus.TraceLevel {
					logger.WithField("namespace", repo.Namespace).
						WithField("name", repo.Name).
						WithField("uid", repo.UID).
						Traceln("syncer: remote repository does not match filter")
				}
			}
		}
	}

	//
	// STEP2: get the list of repositories stored in the
	// local database.
	//

	{
		repos, err := s.repos.List(ctx, user.ID)
		if err != nil {
			logger = logger.WithError(err)
			logger.Warnln("syncer: cannot get cached repository list")
			return nil, err
		}

		for _, repo := range repos {
			local[repo.UID] = repo
		}
	}

	//
	// STEP3 find repos that exist in the remote system,
	// but do not exist locally. Insert.
	//

	for k, v := range remote {
		_, ok := local[k]
		if ok {
			continue
		}
		v.Synced = time.Now().Unix()
		v.Created = time.Now().Unix()
		v.Updated = time.Now().Unix()
		v.Version = 1
		batch.Insert = append(batch.Insert, v)

		if logrus.GetLevel() == logrus.TraceLevel {
			logger.WithField("namespace", v.Namespace).
				WithField("name", v.Name).
				WithField("uid", v.UID).
				Traceln("syncer: remote repository not in database")
		}
	}

	//
	// STEP4 find repos that exist in the remote system and
	// in the local system, but with incorrect data. Update.
	//

	for k, v := range local {
		vv, ok := remote[k]
		if !ok {
			continue
		}
		if diff(v, vv) {
			merge(v, vv)
			v.Synced = time.Now().Unix()
			v.Updated = time.Now().Unix()
			batch.Update = append(batch.Update, v)

			if logrus.GetLevel() == logrus.TraceLevel {
				logger.WithField("namespace", v.Namespace).
					WithField("name", v.Name).
					WithField("uid", v.UID).
					Traceln("syncer: repository requires update")
			}
		}
	}

	//
	// STEP5 find repos that exist in the local system,
	// but not in the remote system. Revoke permissions.
	//

	for k, v := range local {
		_, ok := remote[k]
		if ok {
			continue
		}
		batch.Revoke = append(batch.Revoke, v)

		if logrus.GetLevel() == logrus.TraceLevel {
			logger.WithField("namespace", v.Namespace).
				WithField("name", v.Name).
				WithField("uid", v.UID).
				Traceln("syncer: repository in database not in remote repository list")
		}
	}

	//
	// STEP6 update the database.
	//

	if err := s.batch.Batch(ctx, user, batch); err != nil {
		logger = logger.WithError(err)
		logger.Warnln("syncer: cannot batch update")
		return nil, err
	}

	logger.Debugln("syncer: finished repository sync")
	return batch, nil
}
