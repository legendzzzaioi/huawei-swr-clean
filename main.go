package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/basic"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/config"
	swr "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/swr/v2"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/swr/v2/model"
	region "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/swr/v2/region"
)

type SWRClient struct {
	Region    string
	AK        string
	SK        string
	swrClient *swr.SwrClient
}

var swrClientInstance *SWRClient

func InitSWRClient(ctx context.Context) error {
	ak := "your AK"
	sk := "your SK"
	regionID := "cn-east-3"
	projectId := "your Project ID"

	cred, err := basic.NewCredentialsBuilder().
		WithAk(ak).
		WithSk(sk).
		WithProjectId(projectId).
		SafeBuild()
	if err != nil {
		return fmt.Errorf("创建凭证失败: %v", err)
	}

	regions, err := region.SafeValueOf(regionID)
	if err != nil {
		return fmt.Errorf("创建华为云区域失败: %v", err)
	}

	hcClient, err := swr.SwrClientBuilder().
		WithRegion(regions).
		WithCredential(cred).
		WithHttpConfig(config.DefaultHttpConfig()).
		SafeBuild()
	if err != nil {
		return fmt.Errorf("创建SWR客户端失败: %v", err)
	}

	swrClientInstance = &SWRClient{
		Region:    regionID,
		AK:        ak,
		SK:        sk,
		swrClient: swr.NewSwrClient(hcClient),
	}

	fmt.Println("SWR客户端初始化成功")
	return nil
}

func (c *SWRClient) GetRepositories(namespace string) ([]model.ShowReposResp, error) {
	limit := "100"
	offset := 0 // 改用 int 直接管理

	var allRepos []model.ShowReposResp
	seen := make(map[string]bool) // 防重

	for {
		offsetStr := fmt.Sprintf("%d", offset)
		request := &model.ListReposDetailsRequest{
			Namespace: &namespace,
			Limit:     &limit,
			Offset:    &offsetStr,
		}

		response, err := c.swrClient.ListReposDetails(request)
		if err != nil {
			return nil, fmt.Errorf("获取镜像仓库列表失败: %v", err)
		}

		if response.Body == nil || len(*response.Body) == 0 {
			break
		}

		batch := *response.Body
		for _, r := range batch {
			key := r.Namespace + "/" + r.Name
			if !seen[key] {
				seen[key] = true
				allRepos = append(allRepos, r)
			}
		}

		// 如果这批不足 100 条，说明已经是最后一页
		if len(batch) < 100 {
			break
		}

		offset += len(batch)
	}

	fmt.Printf("找到 %d 个镜像仓库\n", len(allRepos))
	return allRepos, nil
}

func (c *SWRClient) GetRepositoryTags(namespace, repository string) ([]model.ShowReposTagResp, error) {
	limit := "100"
	offset := 0

	var allTags []model.ShowReposTagResp

	for {
		offsetStr := fmt.Sprintf("%d", offset)
		request := &model.ListRepositoryTagsRequest{
			Namespace:  namespace,
			Repository: repository,
			Limit:      &limit,
			Offset:     &offsetStr,
		}

		response, err := c.swrClient.ListRepositoryTags(request)
		if err != nil {
			return nil, fmt.Errorf("获取镜像tag列表失败: %v", err)
		}

		if response.Body == nil || len(*response.Body) == 0 {
			break
		}

		batch := *response.Body
		allTags = append(allTags, batch...)

		if len(batch) < 100 {
			break
		}

		offset += len(batch)
	}

	return allTags, nil
}

func (c *SWRClient) DeleteRepoTag(namespace, repository, tag string) error {
	request := &model.DeleteRepoTagRequest{
		Namespace:  namespace,
		Repository: repository,
		Tag:        tag,
	}

	// 最多重试3次
	for i := 0; i < 3; i++ {
		_, err := c.swrClient.DeleteRepoTag(request)
		if err == nil {
			return nil
		}

		// 429 限流，等待后重试
		if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "ratelimit") {
			waitTime := time.Duration(i+1) * 30 * time.Second
			fmt.Printf("触发限流，等待 %v 后重试 (%d/3)...\n", waitTime, i+1)
			time.Sleep(waitTime)
			continue
		}

		// 其他错误直接返回
		return fmt.Errorf("删除镜像tag %s 失败: %v", tag, err)
	}

	return fmt.Errorf("删除镜像tag %s 失败: 重试3次后仍触发限流", tag)
}

func (c *SWRClient) KeepLatestNTags(namespace, repository string, keepCount int) error {
	tags, err := c.GetRepositoryTags(namespace, repository)
	if err != nil {
		return err
	}

	if len(tags) <= keepCount {
		fmt.Printf("仓库 %s/%s 只有 %d 个tag，无需删除\n", namespace, repository, len(tags))
		return nil
	}

	sort.Slice(tags, func(i, j int) bool {
		return tags[i].Updated > tags[j].Updated
	})

	tagsToDelete := tags[keepCount:]
	fmt.Printf("仓库 %s/%s 共 %d 个tag，保留 %d 个，删除 %d 个\n",
		namespace, repository, len(tags), keepCount, len(tagsToDelete))

	var deleteErrors []string
	for idx, tag := range tagsToDelete {
		if tag.Tag == "" {
			continue
		}
		fmt.Printf("正在删除 (%d/%d): %s\n", idx+1, len(tagsToDelete), tag.Tag)
		if err := c.DeleteRepoTag(namespace, repository, tag.Tag); err != nil {
			msg := fmt.Sprintf("删除 %s 失败: %v", tag.Tag, err)
			deleteErrors = append(deleteErrors, msg)
			fmt.Println(msg)
		} else {
			fmt.Printf("成功删除: %s\n", tag.Tag)
		}

		// 每删除一个 tag 后固定等待 600ms，避免超出 120次/分钟 的限制
		time.Sleep(600 * time.Millisecond)
	}

	if len(deleteErrors) > 0 {
		return fmt.Errorf("部分删除失败: %s", strings.Join(deleteErrors, "; "))
	}
	return nil
}

func (c *SWRClient) CleanupOldTags(namespace string, keepCount int) error {
	repos, err := c.GetRepositories(namespace)
	if err != nil {
		return err
	}

	if len(repos) == 0 {
		fmt.Printf("组织 %s 下没有找到镜像仓库\n", namespace)
		return nil
	}

	fmt.Printf("开始清理组织 %s 下的 %d 个镜像仓库\n\n", namespace, len(repos))

	var allErrors []string
	for _, repo := range repos {
		// Name 是 string 类型，直接使用
		if repo.Name == "" {
			continue
		}
		fmt.Printf("--- 处理仓库: %s ---\n", repo.Name)
		if err := c.KeepLatestNTags(namespace, repo.Name, keepCount); err != nil {
			msg := fmt.Sprintf("处理仓库 %s 失败: %v", repo.Name, err)
			allErrors = append(allErrors, msg)
			fmt.Println(msg)
		}
		fmt.Println()
	}

	if len(allErrors) > 0 {
		return fmt.Errorf("清理过程中出现错误: %s", strings.Join(allErrors, "; "))
	}
	return nil
}

// parseContentRange 解析 "0-99/200" 格式，返回 (total, nextOffset)
func parseContentRange(contentRange string) (total int, nextOffset int) {
	parts := strings.Split(contentRange, "/")
	if len(parts) != 2 {
		return 0, 0
	}
	fmt.Sscanf(parts[1], "%d", &total)
	rangeParts := strings.Split(parts[0], "-")
	if len(rangeParts) == 2 {
		var end int
		fmt.Sscanf(rangeParts[1], "%d", &end)
		nextOffset = end + 1
	}
	return total, nextOffset
}

func main() {
	ctx := context.Background()

	if err := InitSWRClient(ctx); err != nil {
		fmt.Printf("初始化失败: %v\n", err)
		return
	}

	namespace := "zzz-test"
	keepCount := 3

	fmt.Printf("开始清理组织 %s，每个仓库保留最新 %d 个tag\n", namespace, keepCount)
	fmt.Println("================================================")

	if err := swrClientInstance.CleanupOldTags(namespace, keepCount); err != nil {
		fmt.Printf("清理完成但有错误: %v\n", err)
	} else {
		fmt.Println("清理完成！")
	}
}
