//
// nntp.go -- nntp interface for peering
//
package srnd

import (
  "bufio"
  "fmt"
  "io"
  "io/ioutil"
  "log"
  "net/textproto"
  "os"
  "strconv"
  "strings"
  "sync"
  "time"
)


type nntpStreamEvent string

func (ev nntpStreamEvent) MessageID() string {
  return strings.Split(string(ev), " ")[1]
}

func (ev nntpStreamEvent) Command() string {
  return strings.Split(string(ev), " ")[0]
}

func nntpTAKETHIS(msgid string) nntpStreamEvent {
  return nntpStreamEvent(fmt.Sprintf("TAKETHIS %s", msgid))
}

func nntpCHECK(msgid string) nntpStreamEvent {
  return nntpStreamEvent(fmt.Sprintf("CHECK %s", msgid))
}

// nntp connection state
type nntpConnection struct {
  // the name of this feed
  name string
  // the mode we are in now
  mode string
  // what newsgroup is currently selected or empty string if none is selected
  group string
  // the policy for federation
  policy FeedPolicy
  // lock help when expecting non pipelined activity
  access sync.Mutex
  
  // ARTICLE <message-id>
  article chan string
  // TAKETHIS/CHECK <message-id>
  stream chan nntpStreamEvent
}

// write out a mime header to a writer
func writeMIMEHeader(wr io.Writer, hdr textproto.MIMEHeader) (err error) {
  // write headers
  for k, vals := range(hdr) {
    for _, val := range(vals) {
      _, err = io.WriteString(wr, fmt.Sprintf("%s: %s\n", k, val))
    }
  }
  // end of headers
  _, err = io.WriteString(wr, "\n")
  return
}

func createNNTPConnection() nntpConnection {
  return nntpConnection{
    article: make(chan string, 32),
    stream: make(chan nntpStreamEvent, 64),
  }
}

// switch modes
func (self *nntpConnection) modeSwitch(mode string, conn *textproto.Conn) (success bool, err error) {
  self.access.Lock()
  mode = strings.ToUpper(mode)
  conn.PrintfLine("MODE %s", mode)
  log.Println("MODE", mode)
  var code int
  code, _, err = conn.ReadCodeLine(-1)
  if code > 200 && code < 300 {
    // accepted mode change
    if len(self.mode) > 0 {
      log.Printf("mode switch %s -> %s", self.mode, mode)
    } else {
      log.Println("switched to mode", mode)
    }
    self.mode = mode
    success = len(self.mode) > 0 
  }
  self.access.Unlock()
  return
}

func (self *nntpConnection) Quit(conn *textproto.Conn) (err error) {
  conn.PrintfLine("QUIT")
  _, _, err = conn.ReadCodeLine(0)
  return
}

// send a banner for inbound connections
func (self *nntpConnection) inboundHandshake(conn *textproto.Conn) (err error) {
  err = conn.PrintfLine("200 Posting Allowed")
  return err
}

// outbound setup, check capabilities and set mode
// returns (supports stream, supports reader) + error
func (self *nntpConnection) outboundHandshake(conn *textproto.Conn) (stream, reader bool, err error) {
  log.Println(self.name, "outbound handshake")
  var code int
  var line string
  for err == nil {
    code, line, err = conn.ReadCodeLine(-1)
    log.Println(self.name, line)
    if err == nil {
      if code == 200 {
        // send capabilities
        log.Println(self.name, "ask for capabilities")
        err = conn.PrintfLine("CAPABILITIES")
        if err == nil {
          // read response
          dr := conn.DotReader()
          r := bufio.NewReader(dr)
          for {
            line, err = r.ReadString('\n')
            if err == io.EOF {
              // we are at the end of the dotreader
              // set err back to nil and break out
              err = nil
              break
            } else if err == nil {
              // we got a line
              if line == "MODE-READER\n" || line == "READER\n" {
                log.Println(self.name, "supports READER")
                reader = true
              } else if line == "STREAMING\n" {
                stream = true
                log.Println(self.name, "supports STREAMING")
              } else if line == "POSTIHAVESTREAMING\n" {
                stream = true
                reader = false
                log.Println(self.name, "is SRNd")
              }
            } else {
              // we got an error
              log.Println("error reading capabilities", err)
              break
            }
          }
          // return after reading
          return
        }
      } else if code == 201 {
        log.Println("feed", self.name,"does not allow posting")
        // we don't do auth yet
        break
      } else {
        continue
      }
    }
  }
  return
}

// handle streaming event
// this function should send only
func (self *nntpConnection) handleStreaming(daemon NNTPDaemon, reader bool, conn *textproto.Conn) (err error) {
  for err == nil {
    ev := <- self.stream
    log.Println(self.name, ev)
    if ValidMessageID(ev.MessageID()) {
      cmd , msgid := ev.Command(), ev.MessageID()
      if cmd == "TAKETHIS" {
        fname := daemon.store.GetFilename(msgid)
        if CheckFile(fname) {
          f, err := os.Open(fname)
          if err == nil {
            err = conn.PrintfLine("%s", ev)
            // time to send
            dw := conn.DotWriter()
            _ , err = io.Copy(dw, f)
            err = dw.Close()
            f.Close()
          }
        } else {
          log.Println(self.name, "didn't send", msgid, "we don't have it locally")
        }
      } else if cmd == "CHECK" {
        conn.PrintfLine("%s", ev)
      } else {
        log.Println("invalid stream command", ev)
      }
    }
  }
  return
}

// check if we want the article given its mime header
// returns empty string if it's okay otherwise an error message
func (self *nntpConnection) checkMIMEHeader(daemon NNTPDaemon, hdr textproto.MIMEHeader) (reason string, err error) {

  newsgroup := hdr.Get("Newsgroups")
  reference := hdr.Get("References")
  msgid := hdr.Get("Message-Id")
  encaddr := hdr.Get("X-Encrypted-Ip")
  torposter := hdr.Get("X-Tor-Poster")
  i2paddr := hdr.Get("X-I2p-Desthash")
  content_type := hdr.Get("Content-Type")
  has_attachment := strings.HasPrefix(content_type, "multipart/mixed")
  pubkey := hdr.Get("X-Pubkey-Ed25519")
  // TODO: allow certain pubkeys?
  is_signed := pubkey != ""
  is_ctl := newsgroup == "ctl" && is_signed
  anon_poster := torposter != "" || i2paddr != "" || encaddr == ""
  
  if ! newsgroupValidFormat(newsgroup) {
    // invalid newsgroup format
    reason = "invalid newsgroup"
    return
  } else if banned, _ := daemon.database.NewsgroupBanned(newsgroup) ; banned {
    reason = "newsgroup banned"
    return
  } else if ! ( ValidMessageID(msgid) || ( reference != "" && ! ValidMessageID(reference) ) ) {
    // invalid message id or reference
    reason = "invalid reference or message id is '" + msgid + "' reference is '"+reference + "'"
    return
  } else if daemon.database.ArticleBanned(msgid) {
    reason = "article banned"
  } else if reference != "" && daemon.database.ArticleBanned(reference) {
    reason = "thread banned"
  } else if daemon.database.HasArticleLocal(msgid) {
    // we already have this article locally
    reason = "have this article locally"
    return
  } else if daemon.database.HasArticle(msgid) {
    // we have already seen this article
    reason = "already seen"
    return
  } else if is_ctl {
    // we always allow control messages
    return 
  } else if anon_poster {
    // this was posted anonymously
    if daemon.allow_anon {
      if has_attachment || is_signed {
        // this is a signed message or has attachment
        if daemon.allow_anon_attachments {
          // we'll allow anon attachments
          return
        } else {
          // we don't take signed messages or attachments posted anonymously
          reason = "no anon signed posts or attachments"
          return
        }
      } else {
        // we allow anon posts that are plain
        return
      }
    } else {
      // we don't allow anon posts of any kind
      reason = "no anon posts allowed"
      return
    }
  } else {
    // check for banned address
    var banned bool
    if encaddr != "" {
      banned, err = daemon.database.CheckEncIPBanned(encaddr)
      if err == nil {
        if banned {
          // this address is banned
          reason = "address banned"
          return
        } else {
          // not banned
          return
        }
      }
    } else {
      // idk wtf
      log.Println(self.name, "wtf? invalid article")
    }
  }
  return
}

func (self *nntpConnection) handleLine(daemon NNTPDaemon, code int, line string, conn *textproto.Conn) (err error) {
  parts := strings.Split(line, " ")
  var msgid string
  if code == 0 && len(parts) > 1 {
    msgid = parts[1]
  } else {
    msgid = parts[0]
  }
  if code == 238 {
    if ValidMessageID(msgid) {
      self.stream <- nntpTAKETHIS(msgid)
    }
    return
  } else if code == 239 {
    // successful TAKETHIS
    log.Println(msgid, "sent via", self.name)
    return
    // TODO: remember success 
  } else if code == 431 {
    // CHECK said we would like this article later
    log.Println("defer sending", msgid, "to", self.name)
    go self.articleDefer(msgid)
  } else if code == 439 {
    // TAKETHIS failed
    log.Println(msgid, "was not sent to", self.name, "denied:", line)
    // TODO: remember denial
  } else if code == 438 {
    // they don't want the article
    // TODO: remeber rejection
  } else {
    // handle command
    parts := strings.Split(line, " ")
    if len(parts) > 1 {
      cmd := parts[0]
      if cmd == "MODE" {
        if parts[1] == "READER" {
          // reader mode
          self.mode = "READER"
          log.Println(self.name, "switched to reader mode")
          conn.PrintfLine("201 No posting Permitted")
        } else if parts[1] == "STREAM" {
          // wut? we're already in streaming mode
          log.Println(self.name, "already in streaming mode")
          conn.PrintfLine("203 Streaming enabled brah")
        } else {
          // invalid
          log.Println(self.name, "got invalid mode request", parts[1])
          conn.PrintfLine("501 invalid mode variant:", parts[1])
        }
      } else if cmd == "QUIT" {
        // quit command
        conn.PrintfLine("")
        // close our connection and return
        conn.Close()
        return
      } else if cmd == "CHECK" {
        // handle check command
        msgid := parts[1]
        // have we seen this article?
        if daemon.database.HasArticle(msgid) {
          // yeh don't want it
          conn.PrintfLine("438 %s", msgid)
        } else if daemon.database.ArticleBanned(msgid) {
          // it's banned we don't want it
          conn.PrintfLine("438 %s", msgid)
        } else {
          // yes we do want it and we don't have it
          conn.PrintfLine("238 %s", msgid)
        }
      } else if cmd == "TAKETHIS" {
        // handle takethis command
        var hdr textproto.MIMEHeader
        var reason string
        // read the article header
        hdr, err = conn.ReadMIMEHeader()
        if err == nil {
          // check the header
          reason, err = self.checkMIMEHeader(daemon, hdr)
          dr := conn.DotReader()
          if len(reason) > 0 {
            // discard, we do not want
            code = 439
            log.Println(self.name, "rejected", msgid, reason)
            _, err = io.Copy(ioutil.Discard, dr)
            err = daemon.database.BanArticle(msgid, reason)
          } else {
            // check if we don't have the rootpost
            reference := hdr.Get("References")
            newsgroup := hdr.Get("Newsgroups")
            if reference != "" && ValidMessageID(reference) && ! daemon.store.HasArticle(reference) && ! daemon.database.IsExpired(reference) {
              log.Println(self.name, "got reply to", reference, "but we don't have it")
              daemon.ask_for_article <- ArticleEntry{reference, newsgroup}
            }
            f := daemon.store.CreateTempFile(msgid)
            if f == nil {
              log.Println(self.name, "discarding", msgid, "we are already loading it")
              // discard
              io.Copy(ioutil.Discard, dr)
            } else {
              // write header
              err = writeMIMEHeader(f, hdr)
              // write body
              _, err = io.Copy(f, dr)
              if err == nil || err == io.EOF {
                f.Close()
                // we gud, tell daemon
                daemon.infeed_load <- msgid
              } else {
                log.Println(self.name, "error reading message", err)
              }
            }
            code = 239
            reason = "gotten"
          }
        } else {
          log.Println(self.name, "error reading mime header:", err)
          code = 439
          reason = "error reading mime header"
        }
        conn.PrintfLine("%d %s %s", code, msgid, reason)
      } else if cmd == "ARTICLE" {
        if ValidMessageID(msgid) {
          if daemon.store.HasArticle(msgid) {
            // we have it yeh
            f, err := os.Open(daemon.store.GetFilename(msgid))
            if err == nil {
              conn.PrintfLine("220 %s", msgid)
              dw := conn.DotWriter()
              _, err = io.Copy(dw, f)
              dw.Close()
              f.Close()
            } else {
              // wtf?!
              conn.PrintfLine("503 idkwtf happened: %s", err.Error())
            }
          } else {
            // we dont got it
            conn.PrintfLine("430 %s", msgid)
          }
        } else {
          // invalid id
          conn.PrintfLine("500 Syntax error")
        }
      } else if cmd == "POST" {
        // handle POST command
        conn.PrintfLine("340 Post it nigguh; end with <CR-LF>.<CR-LF>")
        hdr, err := conn.ReadMIMEHeader()
        var success bool
        if err == nil {
          hdr["Message-ID"] = []string{genMessageID(daemon.instance_name)}
          reason, err := self.checkMIMEHeader(daemon, hdr)
          success = reason == "" && err == nil
          if success {
            dr := conn.DotReader()
            reference := hdr.Get("References")
            newsgroup := hdr.Get("Newsgroups")
            if reference != "" && ValidMessageID(reference) && ! daemon.store.HasArticle(reference) && ! daemon.database.IsExpired(reference) {
              log.Println(self.name, "got reply to", reference, "but we don't have it")
              daemon.ask_for_article <- ArticleEntry{reference, newsgroup}
            }
            f := daemon.store.CreateTempFile(msgid)
            if f == nil {
              log.Println(self.name, "discarding", msgid, "we are already loading it")
              // discard
              io.Copy(ioutil.Discard, dr)
            } else {
              // write header
              err = writeMIMEHeader(f, hdr)
              // write body
              _, err = io.Copy(f, dr)
              if err == nil || err == io.EOF {
                f.Close()
                // we gud, tell daemon
                daemon.infeed_load <- msgid
              } else {
                log.Println(self.name, "error reading message", err)
              }
            }
          }
        }
        if success && err == nil {
          // all gud
          conn.PrintfLine("240 We got it, thnkxbai")
        } else {
          // failed posting
          if err != nil {
            log.Println(self.name, "failed nntp POST", err)
          }
          conn.PrintfLine("441 Posting Failed")
        }        
      } else if cmd == "IHAVE" {
        // handle IHAVE command
        msgid := parts[1]
        if daemon.database.HasArticleLocal(msgid) || daemon.database.HasArticle(msgid) || daemon.database.ArticleBanned(msgid) {
          // we don't want it
          conn.PrintfLine("435 Article Not Wanted")
        } else {
          // gib we want
          conn.PrintfLine("335 Send it plz")
          hdr, err := conn.ReadMIMEHeader()
          if err == nil {
            // check the header
            var reason string
            reason, err = self.checkMIMEHeader(daemon, hdr)
            dr := conn.DotReader()
            if len(reason) > 0 {
              // discard, we do not want
              log.Println(self.name, "rejected", msgid, reason)
              _, err = io.Copy(ioutil.Discard, dr)
              // ignore this
              _ = daemon.database.BanArticle(msgid, reason)
              conn.PrintfLine("437 Rejected do not send again bro")
            } else {
              // check if we don't have the rootpost
              reference := hdr.Get("References")
              newsgroup := hdr.Get("Newsgroups")
              if reference != "" && ValidMessageID(reference) && ! daemon.store.HasArticle(reference) && ! daemon.database.IsExpired(reference) {
                log.Println(self.name, "got reply to", reference, "but we don't have it")
                daemon.ask_for_article <- ArticleEntry{reference, newsgroup}
              }
              f := daemon.store.CreateTempFile(msgid)
              if f == nil {
                log.Println(self.name, "discarding", msgid, "we are already loading it")
                // discard
                io.Copy(ioutil.Discard, dr)
              } else {
                // write header
                err = writeMIMEHeader(f, hdr)
                // write body
                _, err = io.Copy(f, dr)
                if err == nil || err == io.EOF {
                  f.Close()
                  // we gud, tell daemon
                  daemon.infeed_load <- msgid
                } else {
                  log.Println(self.name, "error reading message", err)
                }
              }
              conn.PrintfLine("235 We got it")
            }
          } else {
            // error here
            conn.PrintfLine("436 Transfer failed: "+err.Error())
          }
        }
      } else if cmd == "NEWSGROUPS" {
        // handle NEWSGROUPS
        conn.PrintfLine("231 List of newsgroups follow")
        dw := conn.DotWriter()
        // get a list of every newsgroup
        groups := daemon.database.GetAllNewsgroups()
        // for each group
        for _, group := range groups {
          // get low/high water mark
          lo, hi, err := daemon.database.GetLastAndFirstForGroup(group)
          if err == nil {
            // XXX: we ignore errors here :\
            _, _ = io.WriteString(dw, fmt.Sprintf("%s %d %d y\n", group, lo, hi))
          } else {
            log.Println(self.name, "could not get low/high water mark for", group, err)
          }
        }
        // flush dotwriter
        dw.Close()
        
      } else if cmd == "XOVER" {
        // handle XOVER
        if self.group == "" {
          conn.PrintfLine("412 No newsgroup selected")
        } else {
          // handle xover command
          // right now it's every article in group
          models, err := daemon.database.GetPostsInGroup(self.group)
          if err == nil {
            conn.PrintfLine("224 Overview information follows")
            dw := conn.DotWriter()
            for idx, model := range models {
              io.WriteString(dw, fmt.Sprintf("%.6d\t%s\t\"%s\" <%s@%s>\t%s\t%s\t%s\r\n", idx+1, model.Subject(), model.Name(), model.Name(), model.Frontend(), model.Date(), model.MessageID(), model.Reference()))
            }
            dw.Close()
          } else {
            log.Println(self.name, "error when getting posts in", self.group, err)
            conn.PrintfLine("500 error, %s", err.Error())
          }
        }
      } else if cmd == "GROUP" {
        // handle GROUP command
        group := parts[1]
        // check for newsgroup
        if daemon.database.HasNewsgroup(group) {
          // we have the group
          self.group = group
          // count posts
          number := daemon.database.CountPostsInGroup(group, 0)
          // get hi/low water marks
          low, hi, err := daemon.database.GetLastAndFirstForGroup(group)
          if err == nil {
            // we gud
            conn.PrintfLine("211 %d %d %d %s", number, low, hi, group)
          } else {
            // wtf error
            log.Println(self.name, "error in GROUP command", err)
            // still have to reply, send it bogus low/hi
            conn.PrintfLine("211 %d 0 1 %s", number, group)
          }
        } else {
          // no such group
          conn.PrintfLine("411 No Such Newsgroup")
        }
      } else {
        log.Println(self.name, "invalid command recv'd", cmd)
        conn.PrintfLine("500 Invalid command: %s", cmd)
      }
    }
  }
  return
}

func (self *nntpConnection) startStreaming(daemon NNTPDaemon, reader bool, conn *textproto.Conn) {
  var err error
  for err == nil {
    err = self.handleStreaming(daemon, reader, conn)
  }
  log.Println(self.name, "error while streaming:", err)
}

// scrape all posts in a newsgroup
// download ones we do not have
func (self *nntpConnection) scrapeGroup(daemon NNTPDaemon, conn *textproto.Conn, group string) (err error) {
  log.Println(self.name, "scrape newsgroup", group)
  // send GROUP command
  err = conn.PrintfLine("GROUP %s", group)
  if err == nil {
    // read reply to GROUP command
    code := 0
    code, _, err = conn.ReadCodeLine(211)
    // check code
    if code == 211 {
      // success
      // send XOVER command, dummy parameter for now
      err = conn.PrintfLine("XOVER 0")
      if err == nil {
        // no error sending command, read first line
        code, _, err = conn.ReadCodeLine(224)
        if code == 224 {
          // maps message-id -> references
          articles := make(map[string]string)
          // successful response, read multiline
          dr := conn.DotReader()
          sc := bufio.NewScanner(dr)
          for sc.Scan() {
            line := sc.Text()
            parts := strings.Split(line, "\t")
            if len(parts) > 5 {
              // probably valid line
              msgid := parts[4]
              // msgid -> reference
              articles[msgid] = parts[5]
            } else {
              // probably not valid line
              // ignore
            }
          }
          err = sc.Err()
          if err == nil {
            // everything went okay when reading multiline
            // for each article
            for msgid, refid := range articles {
              // check the reference
              if len(refid) > 0 && ValidMessageID(refid) {
                // do we have it?
                if daemon.database.HasArticle(refid) {
                  // we have it don't do anything
                } else if daemon.database.ArticleBanned(refid) {
                  // thread banned
                } else {
                  // we don't got root post and it's not banned, try getting it
                  err = self.requestArticle(daemon, conn, refid)
                  if err != nil {
                    // something bad happened
                    log.Println(self.name, "failed to obtain root post", refid, err)
                    return
                  }
                }
              }
              // check the actual message-id
              if len(msgid) > 0 && ValidMessageID(msgid) {
                // do we have it?
                if daemon.database.HasArticle(msgid) {
                  // we have it, don't do shit 
                } else if daemon.database.ArticleBanned(msgid) {
                  // this article is banned, don't do shit yo
                } else {
                  // we don't have it but we want it
                  err = self.requestArticle(daemon, conn, msgid)
                  if err != nil {
                    // something bad happened
                    log.Println(self.name, "failed to obtain article", msgid, err)
                    return
                  }
                }
              }
            }
          } else {
            // something bad went down when reading multiline
            log.Println(self.name, "failed to read multiline for", group, "XOVER command")
          }
        }
      }
    } else if err == nil {
      // invalid response code no error
      log.Println(self.name, "says they don't have", group, "but they should")
    } else {
      // error recving response
      log.Println(self.name, "error recving response from GROUP command", err)
    }
  }
  return
}

// grab every post from the remote server, assumes outbound connection
func (self *nntpConnection) scrapeServer(daemon NNTPDaemon, conn *textproto.Conn) (err error) {
  log.Println(self.name, "scrape remote server")
  success := true
  if success {
    // send newsgroups command
    err = conn.PrintfLine("NEWSGROUPS %d 000000 GMT", timeNow())
    if err == nil {
      // read response line
      code, _, err := conn.ReadCodeLine(231)
      if code == 231 {
        var groups []string
        // valid response, we expect a multiline
        dr := conn.DotReader()
        // read lines
        sc := bufio.NewScanner(dr)
        for sc.Scan() {
          line := sc.Text()
          idx := strings.Index(line, " ")
          if idx > 0 {
            groups = append(groups, line[:idx])
          } else {
            // invalid line? wtf.
            log.Println(self.name, "invalid line in newsgroups multiline response:", line)
          }
        }
        err = sc.Err()
        if err == nil {
          log.Println(self.name, "got list of newsgroups")
          // for each group
          for _, group := range groups {
            var banned bool
            // check if the newsgroup is banned
            banned, err = daemon.database.NewsgroupBanned(group)
            if banned {
              // we don't want it
            } else if err == nil {
              // scrape the group
              err = self.scrapeGroup(daemon, conn, group)
              if err != nil {
                log.Println(self.name, "did not scrape", group, err)
                break
              }
            } else {
              // error while checking for ban
              log.Println(self.name, "checking for newsgroup ban failed", err)
              break
            }
          }
        } else {
          // we got a bad multiline block?
          log.Println(self.name, "bad multiline response from newsgroups command", err)
        }
      } else if err == nil {
        // invalid response no error
        log.Println(self.name, "gave us invalid response to newsgroups command", code)
      } else {
        // invalid response with error
        log.Println(self.name, "error while reading response from newsgroups command", err)
      }
    } else {
      log.Println(self.name, "failed to send newsgroups command", err)
    }
  } else if err == nil {
    // failed to switch mode to reader
    log.Println(self.name, "does not do reader mode, bailing scrape")
  } else {
    // failt to switch mode because of error
    log.Println(self.name, "failed to switch to reader mode when scraping", err)
  }
  return
}

// ask for an article from the remote server
// feed it to the daemon if we get it
func (self *nntpConnection) requestArticle(daemon NNTPDaemon, conn *textproto.Conn, msgid string) (err error) {
  log.Println(self.name, "asking for", msgid)
  // send command
  err = conn.PrintfLine("ARTICLE %s", msgid)
  // read response
  code, line, err := conn.ReadCodeLine(-1)
  if code == 220 {
    // awwww yeh we got it
    var hdr textproto.MIMEHeader
    // read header
    hdr, err = conn.ReadMIMEHeader()
    if err == nil {
      // prepare to read body
      dr := conn.DotReader()
      // check header and decide if we want this
      reason, err := self.checkMIMEHeader(daemon, hdr)
      if err == nil {
        if len(reason) > 0 {
          log.Println(self.name, "discarding", msgid, reason)
          // we don't want it, discard
          io.Copy(ioutil.Discard, dr)
          daemon.database.BanArticle(msgid, reason)
        } else {
          // yeh we want it open up a file to store it in
          f := daemon.store.CreateTempFile(msgid)
          if f == nil {
            // already being loaded elsewhere
          } else {
            // write header to file
            writeMIMEHeader(f, hdr)
            // write article body to file
            _, _ = io.Copy(f, dr)
            // close file
            f.Close()
            log.Println(msgid, "obtained via reader from", self.name)
            // tell daemon to load article via infeed
            daemon.infeed_load <- msgid
          }
        }
      } else {
        // error happened while processing
        log.Println(self.name, "error happend while processing MIME header", err)
      }
    } else {
      // error happened while reading header
      log.Println(self.name, "error happened while reading MIME header", err)
    }
  } else if code == 430 {
    // they don't know it D:
    log.Println(msgid, "not known by", self.name)
  } else {
    // invalid response
    log.Println(self.name, "invald response to ARTICLE:", code, line)
  }
  return
}

func (self *nntpConnection) startReader(daemon NNTPDaemon, conn *textproto.Conn) {
  log.Println(self.name, "run reader mode")
  var err error
  for err == nil {
    // next article to ask for
    msgid := <- self.article
    err = self.requestArticle(daemon, conn, msgid)
  }
  // report error and close connection
  log.Println(self.name, "error while in reader mode:", err)
  conn.Close()
}

// run the mainloop for this connection
// stream if true means they support streaming mode
// reader if true means they support reader mode
func (self *nntpConnection) runConnection(daemon NNTPDaemon, inbound, stream, reader bool, preferMode string, conn *textproto.Conn) {

  var err error
  var line string
  var success bool

  for err == nil {
    if self.mode == "" {
      if inbound  {
        // no mode and inbound
        line, err = conn.ReadLine()
        if len(line) == 0 {
          conn.Close()
          return
        } else if line == "QUIT" {
          conn.PrintfLine("205 bai")
          conn.Close()
          return
        }
        parts := strings.Split(line, " ")
        cmd := parts[0]
        if cmd == "CAPABILITIES" {
          // write capabilities
          conn.PrintfLine("101 i support to the following:")
          dw := conn.DotWriter()
          caps := []string{"VERSION 2", "READER", "STREAMING", "IMPLEMENTATION srndv2"}
          for _, cap := range caps {
            io.WriteString(dw, cap)
            io.WriteString(dw, "\n")
          }
          dw.Close()
          log.Println(self.name, "sent Capabilities")
        } else if cmd == "MODE" {
          if len(parts) == 2 {
            if parts[1] == "READER" {
              // set reader mode
              self.mode = "READER"
              // we'll allow posting for reader
              conn.PrintfLine("201 Not Posting Permitted Yo")
            } else if parts[1] == "STREAM" {
              // set streaming mode
              conn.PrintfLine("203 Stream it brah")
              self.mode = "STREAM"
              log.Println(self.name, "streaming enabled")
              go self.startStreaming(daemon, reader, conn)
            }
          }
        } else {
          // handle a it as a command, we don't have a mode set
          parts := strings.Split(line, " ")
          var code64 int64
          code64, err = strconv.ParseInt(parts[0], 10, 32)
          if err ==  nil {
            err = self.handleLine(daemon, int(code64), line[4:], conn)
          } else {
            err = self.handleLine(daemon, 0, line, conn)
          }
        }
      } else { // no mode and outbound
        if preferMode == "stream" {
          // try outbound streaming
          if stream {
            success, err = self.modeSwitch("STREAM", conn)
            self.mode = "STREAM"
            if success {
              // start outbound streaming in background
              go self.startStreaming(daemon, reader, conn)
            }
          }
        } else if reader {
          // try reader mode
          success, err = self.modeSwitch("READER", conn)
          if success {
            self.mode = "READER"
            self.startReader(daemon, conn)
          }
        }
        if success {
          log.Println(self.name, "mode set to", self.mode)
        } else {
          // bullshit
          // we can't do anything so we quit
          log.Println(self.name, "can't stream or read, wtf?")
          conn.PrintfLine("QUIT")
          conn.Close()
          return
        }
      }
    } else {
      // we have our mode set
      line, err = conn.ReadLine()
      if err == nil {
        parts := strings.Split(line, " ")
        var code64 int64
        code64, err = strconv.ParseInt(parts[0], 10, 32)
        if err ==  nil {
          err = self.handleLine(daemon, int(code64), line[4:], conn)
        } else {
          err = self.handleLine(daemon, 0, line, conn)
        }
      }
    }
  }
  log.Println(self.name, "got error", err)
  if ! inbound {
    // send quit on outbound
    conn.PrintfLine("QUIT")
  }
  conn.Close()
}

func (self *nntpConnection) articleDefer(msgid string) {
  time.Sleep(time.Second * 90)
  self.stream <- nntpCHECK(msgid)
}
