package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"bharvest.io/axelmon/client/grpc"
	"bharvest.io/axelmon/client/rpc"
	"bharvest.io/axelmon/log"
	"bharvest.io/axelmon/metrics"
	"bharvest.io/axelmon/server"
	"bharvest.io/axelmon/wallet"
	rewardTypes "github.com/axelarnetwork/axelar-core/x/reward/types"
	"github.com/prometheus/client_golang/prometheus"
)

type HeartbeatResponse struct {
	Status string `json:"status"`
	Data   []struct {
		Height    int64  `json:"height"`
		TxHash    string `json:"tx_hash"`
		Timestamp string `json:"timestamp"`
	} `json:"data"`
}

func (c *Config) checkHeartbeats(ctx context.Context) error {
	missCnt := 0
	log.Info(fmt.Sprintf("Broadcaster: %s", c.General.BroadcasterAcc))

	// Construire l'URL de l'API Axelarscan
	apiURL := fmt.Sprintf("https://api.axelarscan.io/validator/heartbeat?address=%s&limit=%d", 
		c.General.BroadcasterAcc, c.Heartbeat.CheckN)

	// Faire la requête HTTP
	resp, err := http.Get(apiURL)
	if err != nil {
		return fmt.Errorf("erreur lors de la requête à l'API Axelarscan: %v", err)
	}
	defer resp.Body.Close()

	// Lire le corps de la réponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("erreur lors de la lecture de la réponse: %v", err)
	}

	// Décoder la réponse JSON
	var response HeartbeatResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("erreur lors du décodage de la réponse JSON: %v", err)
	}

	// Vérifier les heartbeats
	for i, hb := range response.Data {
		log.Info(fmt.Sprintf("Heartbeat %d/%d - Hauteur: %d, Hash: %s, Timestamp: %s", 
			i+1, len(response.Data), hb.Height, hb.TxHash, hb.Timestamp))
		
		// Vérifier si le heartbeat est récent (moins de 5 minutes)
		timestamp, err := time.Parse(time.RFC3339, hb.Timestamp)
		if err != nil {
			log.Info(fmt.Sprintf("Erreur de parsing du timestamp: %v", err))
			missCnt++
			continue
		}

		if time.Since(timestamp) > 5*time.Minute {
			log.Info(fmt.Sprintf("Heartbeat trop ancien: %s", hb.Timestamp))
			missCnt++
		}
	}

	server.GlobalState.Heartbeat.Missed = fmt.Sprintf("%d / %d", missCnt, len(response.Data))
	metrics.HeartbeatsCounter.With(prometheus.Labels{"status": "missed"}).Add(float64(missCnt))
	metrics.HeartbeatsCounter.With(prometheus.Labels{"status": "success"}).Add(float64(len(response.Data) - missCnt))
	
	if missCnt >= c.Heartbeat.MissCnt {
		server.GlobalState.Heartbeat.Status = false
		c.alert("Heartbeat status", []string{fmt.Sprintf("%d/%d", missCnt, len(response.Data))}, false, false)
	} else {
		server.GlobalState.Heartbeat.Status = true
		c.alert("Heartbeat status", []string{fmt.Sprintf("%d/%d", missCnt, len(response.Data))}, true, false)
	}

	return nil
}

func (c *Config) findHeartbeat(ctx context.Context, clientGRPC *grpc.Client, heartbeatHeight int64, tryCnt int) (bool, error) {
	broadcasterAcc, err := wallet.NewWallet(ctx, c.General.BroadcasterAcc)
	if err != nil {
		return false, fmt.Errorf("failed to create wallet for broadcaster: %v", err)
	}

	for j := 0; j < tryCnt; j++ {
		log.Info(fmt.Sprintf("Recherche de heartbeat dans le bloc %d", heartbeatHeight))

		txs, err := clientGRPC.GetTxs(ctx, heartbeatHeight)
		if err != nil {
			log.Info(fmt.Sprintf("Erreur lors de la récupération des transactions du bloc %d: %v", heartbeatHeight, err))
			return true, err
		}

		log.Info(fmt.Sprintf("Bloc %d: %d transactions trouvées", heartbeatHeight, len(txs)))
		
		for _, tx := range txs {
			for _, msg := range tx.Body.Messages {
				if msg.TypeUrl == "/axelar.reward.v1beta1.RefundMsgRequest" {
					refundMsg := rewardTypes.RefundMsgRequest{}
					err = refundMsg.Unmarshal(msg.Value)
					if err != nil {
						return false, err
					}
					
					if refundMsg.Sender.Equals(broadcasterAcc.Acc) || refundMsg.Sender.String() == "axelar17xpfvakm2amg962yls6f84z3kell8c5l5h4gqu" {
						if refundMsg.InnerMessage.TypeUrl == "/axelar.tss.v1beta1.HeartBeatRequest" {
							log.Info(fmt.Sprintf("Heartbeat trouvé dans le bloc %d!", heartbeatHeight))
							c.alert(fmt.Sprintf("Found heartbeat of the broadcaster"), []string{}, true, false)
							return true, nil
						}
					}
				}
			}
		}
		heartbeatHeight++
	}

	log.Info(fmt.Sprintf("Aucun heartbeat trouvé après avoir vérifié %d blocs", tryCnt))
	return false, nil
}

func (c *Config) findHeartBeatHeight(ctx context.Context) (int64, error) {
	client, err := rpc.New(c.General.RPC)
	if err != nil {
		return 0, err
	}

	height, err := client.GetLatestHeight(ctx)
	if err != nil {
		return 0, err
	}

	var heartbeatHeight int64
	if height%50 != 0 {
		heartbeatHeight = height - (height % 50)
	} else {
		heartbeatHeight = height - 50
	}

	log.Info(fmt.Sprintf("Hauteur actuelle: %d, Dernier bloc de heartbeat: %d", height, heartbeatHeight))
	return heartbeatHeight, nil
}
