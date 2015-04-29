//
// store.go
//

package srnd

import (
  "errors"
  "log"
  "os"
  "path/filepath"
)
// iterator hook function
// return true on error
type StoreIteratorHook func (fname string) bool

type ArticleStore struct {
  directory string
  database *Database
}

// initialize article store
func (self *ArticleStore) Init() {
  EnsureDir(self.directory)
}

func (self *ArticleStore) StoreArticle(newsgroup string, article string) error {
  var err error
  group := filepath.Clean(newsgroup)
  apath := filepath.Clean(article)
  fpath := filepath.Join(self.directory, group)
  fpath, err = filepath.Abs(fpath)
  EnsureDir(fpath)
  newpath := filepath.Join(fpath, apath)
  if err != nil {
    log.Println("failed to make symlinks", err)
    return err
  }
  if CheckFile(newpath) {
    log.Println("already symlinked", newpath)
    return nil
  }
  err = os.Symlink("../"+apath, newpath)
  if err != nil {
    log.Println("failed to symlink", err) 
  } else {
    log.Println("stored article", article, "in" , newsgroup)
  }
  return err
}

func (self *ArticleStore) IterateAllForNewsgroup(newsgroup string, hook StoreIteratorHook) error {
  
  group := filepath.Clean(newsgroup)
  
  fpath := filepath.Join(self.directory, group)
  f, err := os.Open(fpath)
  if err != nil {
    return err
  }
  var names []string 
  names, err = f.Readdirnames(-1)
  for idx := range(names) {
    fname := names[idx]
    if hook(fname) {
      break
    }
  }
  return err
}

// iterate over the articles in this article store
// call a hookfor each article passing in the messageID
func (self *ArticleStore) IterateAllArticles(hook StoreIteratorHook) error {
  f , err := os.Open(self.directory)
  if err != nil {
    return err
  }
  var names []string
  names, err = f.Readdirnames(-1)
  for idx := range names {
    fname := names[idx]
    if IsDir(self.GetFilename(fname)) {
      continue
    }
    if hook(fname) {
      break
    }
  }
  f.Close()
  return nil
}

// create a file for this article
func (self *ArticleStore) CreateFile(messageID string) *os.File {
  fname := self.GetFilename(messageID)
  file, err := os.Create(fname)
  if err != nil {
    //log.Fatal("cannot open file", fname)
    return nil
  }
  return file
}

// store article from frontend
func (self *ArticleStore) StorePost(post *NNTPMessage) error {
  file := self.CreateFile(post.MessageID)
  if file == nil {
    return errors.New("cannot open file for post "+post.MessageID)
  }
  post.WriteTo(file)
  file.Close()
  return nil
}

// return true if we have an article
func (self *ArticleStore) HasArticle(messageID string) bool {
  return CheckFile(self.GetFilename(messageID))
}

// get the filename for this article
func (self *ArticleStore) GetFilename(messageID string) string {
  return filepath.Join(self.directory, messageID)
}

// load a file into a message
// pass true to load the body too
// return nil on failure
func (self *ArticleStore) GetMessage(messageID string, loadBody bool) *NNTPMessage {
  fname := self.GetFilename(messageID)
  file, err := os.Open(fname)
  if err != nil {
    //log.Fatal("cannot open",fname)
    return nil
  }
  message := new(NNTPMessage)
  success := message.Load(file, loadBody)
  file.Close()
  if success {
    return message
  }
  log.Println("failed to load file", fname)
  return nil
}