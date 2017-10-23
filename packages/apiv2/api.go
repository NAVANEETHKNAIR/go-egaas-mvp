// Copyright 2016 The go-daylight Authors
// This file is part of the go-daylight library.
//
// The go-daylight library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-daylight library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-daylight library. If not, see <http://www.gnu.org/licenses/>.

package apiv2

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/EGaaS/go-egaas-mvp/packages/config"
	"github.com/EGaaS/go-egaas-mvp/packages/consts"
	"github.com/EGaaS/go-egaas-mvp/packages/converter"
	"github.com/EGaaS/go-egaas-mvp/packages/model"
	"github.com/EGaaS/go-egaas-mvp/packages/utils"
	"github.com/EGaaS/go-egaas-mvp/packages/utils/tx"
	"github.com/dgrijalva/jwt-go"
	hr "github.com/julienschmidt/httprouter"
	log "github.com/sirupsen/logrus"
	"gopkg.in/vmihailenco/msgpack.v2"
)

const (
	jwtPrefix = "Bearer "
	jwtExpire = 36000 // By default, seconds
)

type apiData struct {
	status int
	result interface{}
	params map[string]interface{}
	state  int64
	wallet int64
	token  *jwt.Token
	//	sess   session.SessionStore
}

type forSign struct {
	Time    string `json:"time"`
	ForSign string `json:"forsign"`
}

type hashTx struct {
	Hash string `json:"hash"`
}

const (
	pInt64 = iota
	pHex
	pString

	pOptional = 0x100
)

type apiHandle func(http.ResponseWriter, *http.Request, *apiData, *log.Entry) error

var (
	installed bool
)

func errorAPI(w http.ResponseWriter, err interface{}, code int, params ...interface{}) error {
	var (
		msg, errCode, errParams string
	)

	switch v := err.(type) {
	case string:
		errCode = v
		if val, ok := errors[v]; ok {
			if len(params) > 0 {
				list := make([]string, 0)
				msg = fmt.Sprintf(val, params...)
				for _, item := range params {
					list = append(list, fmt.Sprintf(`"%v"`, item))
				}
				errParams = fmt.Sprintf(`, "params": [%s]`, strings.Join(list, `,`))
			} else {
				msg = val
			}
		} else {
			msg = v
		}
	case interface{}:
		errCode = `E_SERVER`
		if reflect.TypeOf(v).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
			msg = v.(error).Error()
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	fmt.Fprintln(w, fmt.Sprintf(`{"error": %q, "msg": %q %s}`, errCode, msg, errParams))
	return fmt.Errorf(msg)
}

func getPrefix(data *apiData) (prefix string) {
	return converter.Int64ToStr(data.state)
}

func getSignHeader(txName string, data *apiData) tx.Header {
	return tx.Header{Type: int(utils.TypeInt(txName)), Time: time.Now().Unix(),
		UserID: data.state, StateID: data.wallet}
}

func getHeader(txName string, data *apiData) (tx.Header, error) {
	publicKey := []byte("null")
	if _, ok := data.params[`pubkey`]; ok && len(data.params[`pubkey`].([]byte)) > 0 {
		publicKey = data.params[`pubkey`].([]byte)
		lenpub := len(publicKey)
		if lenpub > 64 {
			publicKey = publicKey[lenpub-64:]
		}
	}
	signature := data.params[`signature`].([]byte)
	if len(signature) == 0 {
		log.WithFields(log.Fields{"params": data.params}).Error("signature is empty")
		return tx.Header{}, fmt.Errorf("signature is empty")
	}
	timeInt, err := strconv.ParseInt(data.params["time"].(string), 10, 64)
	if err != nil {
		log.WithFields(log.Fields{"type": consts.ConvertionError, "val": data.params["time"], "error": err}).Error("converting http param time to int")
	}
	return tx.Header{Type: int(utils.TypeInt(txName)), Time: timeInt,
		UserID: data.wallet, StateID: data.state, PublicKey: publicKey,
		BinSignatures: converter.EncodeLengthPlusData(signature)}, nil
}

func sendEmbeddedTx(txType int, userID int64, toSerialize interface{}) (*hashTx, error) {
	var hash []byte
	serializedData, err := msgpack.Marshal(toSerialize)
	if err != nil {
		log.WithFields(log.Fields{"type": consts.MarshallingError, "error": err}).Error("send embedded tx marshall to msgpack")
		return nil, err
	}
	if hash, err = model.SendTx(int64(txType), userID,
		append(converter.DecToBin(int64(txType), 1), serializedData...)); err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("sending tx to the queue")
		return nil, err
	}
	return &hashTx{Hash: string(converter.BinToHex(hash))}, nil
}

// DefaultHandler is a common handle function for api requests
func DefaultHandler(params map[string]int, handlers ...apiHandle) hr.Handle {
	return hr.Handle(func(w http.ResponseWriter, r *http.Request, ps hr.Params) {
		var (
			err  error
			data apiData
		)
		requestLogger := log.WithFields(log.Fields{"headers": r.Header, "path": r.URL.Path, "protocol": r.Proto, "remote": r.RemoteAddr})
		requestLogger.Info("received http request")
		defer func() {
			if r := recover(); r != nil {
				requestLogger.WithFields(log.Fields{"type": consts.PanicRecoveredError, "error": r, "stack": debug.Stack()}).Error("panic recovered error")
				errorAPI(w, `E_RECOVERED`, http.StatusInternalServerError)
			}
		}()
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !installed && r.URL.Path != `/api/v2/install` {
			if model.DBConn == nil && !config.IsExist() {
				errorAPI(w, `E_NOTINSTALLED`, http.StatusInternalServerError)
				return
			}
			installed = true
		}
		token, err := jwtToken(r)
		if err != nil {
			requestLogger.WithFields(log.Fields{"type": consts.SessionError, "params": params, "error": err}).Error("starting session")
			errorAPI(w, err, http.StatusBadRequest)
			return
		}
		data.token = token
		if token != nil && token.Valid {
			if claims, ok := token.Claims.(*JWTClaims); ok && len(claims.Wallet) > 0 {
				stateInt, err := strconv.ParseInt(claims.State, 10, 64)
				if err != nil {
					requestLogger.WithFields(log.Fields{"type": consts.ConvertionError, "error": err, "value": claims.State}).Warning("converting state to int failed, using 0")
					stateInt = 0
				}
				walletInt, err := strconv.ParseInt(claims.Wallet, 10, 64)
				if err != nil {
					requestLogger.WithFields(log.Fields{"type": consts.ConvertionError, "error": err, "value": claims.Wallet}).Warning("converting wallet to int failed, using 0")
					walletInt = 0
				}
				data.state = stateInt
				data.wallet = walletInt
			}
		}
		// Getting and validating request parameters
		r.ParseForm()
		data.params = make(map[string]interface{})
		for _, par := range ps {
			data.params[par.Key] = par.Value
		}
		for key, par := range params {
			val := r.FormValue(key)
			if par&pOptional == 0 && len(val) == 0 {
				requestLogger.WithFields(log.Fields{"type": consts.RouteError, "error": fmt.Sprintf("undefined val %s", key)}).Error("undefined val")
				errorAPI(w, `E_UNDEFINEVAL`, http.StatusBadRequest, key)
				return
			}
			switch par & 0xff {
			case pInt64:
				data.params[key], err = strconv.ParseInt(val, 10, 64)
				if err != nil {
					requestLogger.WithFields(log.Fields{"type": consts.ConvertionError, "value": val, "error": err}).Error("converting http parameter to int")
				}
			case pHex:
				bin, err := hex.DecodeString(val)
				if err != nil {
					requestLogger.WithFields(log.Fields{"type": consts.ConvertionError, "value": val, "error": err}).Error("decoding http parameter from hex")
					errorAPI(w, err, http.StatusBadRequest)
					return
				}
				data.params[key] = bin
			case pString:
				data.params[key] = val
			}
		}
		for _, handler := range handlers {
			if handler(w, r, &data, requestLogger) != nil {
				return
			}
		}
		jsonResult, err := json.Marshal(data.result)
		if err != nil {
			requestLogger.WithFields(log.Fields{"type": consts.JSONMarshallError, "error": err}).Error("marhsalling http response to json")
			errorAPI(w, err, http.StatusInternalServerError)
			return
		}
		w.Write(jsonResult)
	})
}

func checkEcosystem(w http.ResponseWriter, data *apiData, logger *log.Entry) (int64, error) {
	state := data.state
	if data.params[`ecosystem`].(int64) > 0 {
		state = data.params[`ecosystem`].(int64)
		count, err := model.GetNextID(`system_states`)
		if err != nil {
			logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting next id system states")
			return 0, errorAPI(w, err, http.StatusBadRequest)
		}
		if state >= count {
			logger.WithFields(log.Fields{"state_id": state, "count": count}).Error("state_id is larger then max count")
			return 0, errorAPI(w, `E_ECOSYSTEM`, http.StatusBadRequest, state)
		}
	}
	return state, nil
}