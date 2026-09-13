package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/elevenlj/iris/internal/session"
	"github.com/elevenlj/iris/internal/store"
)

func (s *botService) PreviewBotDeletion(ctx context.Context, id string) (httpapi.BotDeletionPreview, error) {
	s.root.mu.Lock()
	defer s.root.mu.Unlock()
	for _, bot := range s.root.cfg.Bots {
		if bot.ID != id {
			continue
		}
		st := s.defaultStore
		if id != "default" {
			rt := s.runtimes[id]
			if rt == nil {
				break
			}
			st = rt.store
		}
		if st == nil {
			break
		}
		list, err := st.ListSessions(ctx)
		if err != nil {
			return httpapi.BotDeletionPreview{}, err
		}
		info := httpapi.BotDeletionPreview{Sessions: len(list)}
		for _, sess := range list {
			if sess.Live && sess.Status == session.StatusRunning {
				info.Running++
			}
		}
		return info, nil
	}
	return httpapi.BotDeletionPreview{}, errors.New("机器人不存在或数据不可用")
}

func (s *botService) DeleteBot(ctx context.Context, id string) (httpapi.BotDeletion, error) {
	// ponytail: serialize config writes during backup; split out a per-bot lock if large backups make settings sluggish.
	s.root.mu.Lock()
	defer s.root.mu.Unlock()
	result := httpapi.BotDeletion{}
	cfg := *s.root.cfg
	next := make([]httpapi.BotConfig, 0, len(cfg.Bots)) // [] persists an intentionally empty bot list.
	var target httpapi.BotConfig
	for _, bot := range cfg.Bots {
		if bot.ID == id {
			target = bot
		} else {
			next = append(next, bot)
		}
	}
	if !validBotID(id) || target.ID == "" {
		return result, errors.New("机器人不存在或已删除")
	}
	base := filepath.Join(s.dataDir, "bots", id)
	rt := s.runtimes[id]
	uploads := uploadsDirInDataDir(base)
	if id == "default" {
		base = s.dataDir
		uploads = s.defaultUploads
		if uploads == "" {
			uploads = uploadsDirInDataDir(base)
		}
		rt = &botRuntime{manager: s.root.manager, bridge: s.root.bridge, store: s.defaultStore}
		cfg.LarkAppID, cfg.LarkAppSecret, cfg.LarkNotifyReceiveID = "", "", ""
	}
	if rt == nil || rt.store == nil {
		return result, errors.New("机器人数据不可用，未执行删除")
	}
	cfg.Bots = next
	committed := false
	var sessions []session.Session
	err := rt.manager.Retire(context.WithoutCancel(ctx), func() error {
		var err error
		sessions, err = rt.store.ListSessions(ctx)
		if err != nil {
			return err
		}
		for _, sess := range sessions {
			if !safeSessionDataID(sess.ID) {
				return errors.New("无效会话 ID，未执行删除")
			}
		}
		result.BackupPath, err = s.backupBot(ctx, target, rt.store, base, uploads, sessions)
		if err != nil {
			return fmt.Errorf("备份失败，未删除机器人：%w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeConfigFile(s.root.path, cfg); err != nil {
			return err
		}
		*s.root.cfg = cfg
		committed = true
		rt.bridge.Retire()
		return nil
	})
	if !committed {
		return result, err
	}
	delete(s.runtimes, id)
	if rt.headless != nil {
		rt.headless.StopAll()
	}
	if id != "default" {
		err = errors.Join(err, rt.store.Close())
	}
	// Never remove the service directory, an Agent's CWD, or an overridden uploads root.
	err = errors.Join(err, os.RemoveAll(filepath.Join(base, "data", "sessions")))
	for _, sess := range sessions {
		// The backup validated IDs before any destructive action.
		err = errors.Join(err, os.RemoveAll(filepath.Join(uploads, sess.ID)))
	}
	if err != nil {
		result.Warning = "机器人已删除，部分本地数据清理失败；备份位于 " + result.BackupPath
		log.Printf("bot delete cleanup id=%s: %v", id, err)
	}
	log.Printf("bot deleted id=%s backup=%s", id, result.BackupPath)
	return result, nil
}

func (s *botService) backupBot(ctx context.Context, bot httpapi.BotConfig, st *store.SQLite, base, uploads string, sessions []session.Session) (string, error) {
	parent := filepath.Join(s.dataDir, "deleted-bots")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, bot.ID+"-")
	if err != nil {
		return "", err
	}
	metadata, err := json.MarshalIndent(bot, "", "  ")
	if err != nil {
		return dir, err
	}
	if err := os.WriteFile(filepath.Join(dir, "bot.json"), metadata, 0600); err != nil {
		return dir, err
	}
	db := filepath.Join(dir, "iris.db")
	if err := st.Backup(ctx, db); err != nil {
		return dir, err
	}
	if err := os.Chmod(db, 0600); err != nil {
		return dir, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "files.tar.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return dir, err
	}
	z := gzip.NewWriter(f)
	tw := tar.NewWriter(z)
	err = archiveBotPath(ctx, tw, filepath.Join(base, "data", "sessions"), "sessions")
	for _, sess := range sessions {
		if err != nil {
			break
		}
		err = archiveBotPath(ctx, tw, filepath.Join(uploads, sess.ID), filepath.Join("uploads", sess.ID))
	}
	err = errors.Join(err, tw.Close(), z.Close(), f.Close())
	return dir, err
}

func safeSessionDataID(id string) bool {
	return id != "" && id != "." && id != ".." && !filepath.IsAbs(id) && !strings.ContainsAny(id, `/\`)
}

// Preserve symlinks as links, never traverse into project directories or shared Agent homes.
func archiveBotPath(ctx context.Context, tw *tar.Writer, root, name string) error {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(filepath.Join(name, rel))
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.CopyN(tw, f, info.Size())
		return errors.Join(err, f.Close())
	})
}
