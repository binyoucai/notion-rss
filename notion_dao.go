package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jomei/notionapi"
)


type NotionDao struct {
	feedDatabaseId    notionapi.DatabaseID
	contentDatabaseId notionapi.DatabaseID
	client            *notionapi.Client
	globalHash map[string]string
}

// ConstructNotionDaoFromEnv given environment variables: NOTION_RSS_KEY,
// NOTION_RSS_CONTENT_DATABASE_ID, NOTION_RSS_FEEDS_DATABASE_ID
func ConstructNotionDaoFromEnv() (*NotionDao, error) {
	integrationKey, exists := os.LookupEnv("NOTION_RSS_KEY")
	if !exists {
		return &NotionDao{}, fmt.Errorf("`NOTION_RSS_KEY` not set")
	}

	contentDatabaseId, exists := os.LookupEnv("NOTION_RSS_CONTENT_DATABASE_ID")
	if !exists {
		return &NotionDao{}, fmt.Errorf("`NOTION_RSS_CONTENT_DATABASE_ID` not set")
	}

	feedDatabaseId, exists := os.LookupEnv("NOTION_RSS_FEEDS_DATABASE_ID")
	if !exists {
		return &NotionDao{}, fmt.Errorf("`NOTION_RSS_FEEDS_DATABASE_ID` not set")
	}

	return ConstructNotionDao(feedDatabaseId, contentDatabaseId, integrationKey), nil
}

func ConstructNotionDao(feedDatabaseId string, contentDatabaseId string, integrationKey string) *NotionDao {
	return &NotionDao{
		feedDatabaseId:    notionapi.DatabaseID(feedDatabaseId),
		contentDatabaseId: notionapi.DatabaseID(contentDatabaseId),
		client:            notionapi.NewClient(notionapi.Token(integrationKey)),
	}
}

// GetOldUnstarredRSSItems that were created strictly before olderThan and are not starred.
func (dao NotionDao) GetOldUnstarredRSSItems(olderThan time.Time) []notionapi.Page {
	resp, err := dao.client.Database.Query(context.TODO(), dao.contentDatabaseId, &notionapi.DatabaseQueryRequest{
		Filter: (notionapi.AndCompoundFilter)([]notionapi.Filter{

			// Use `Created`, not `Published` as to avoid deleting cold-started RSS feeds.
			notionapi.PropertyFilter{
				Property: "Created",
				Date: &notionapi.DateFilterCondition{
					Before: (*notionapi.Date)(&olderThan),
				},
			},
			notionapi.PropertyFilter{
				Property: "Starred",
				Checkbox: &notionapi.CheckboxFilterCondition{
					Equals:       false,
					DoesNotEqual: true,
				},
			},
		}),
		// TODO: pagination
		//StartCursor:    "",
		//PageSize:       0,
	})
	if err != nil {
		fmt.Printf("error occurred in GetOldUnstarredRSSItems. Error: %s\n", err.Error())
		return []notionapi.Page{}
	}
	return resp.Results
}

func (dao NotionDao) GetOldUnstarredRSSItemIds(olderThan time.Time) []notionapi.PageID {
	pages := dao.GetOldUnstarredRSSItems(olderThan)
	result := make([]notionapi.PageID, len(pages))
	for i, page := range pages {
		result[i] = notionapi.PageID(page.ID)
	}
	return result
}

// ArchivePages for a list of pageIds. Will archive each page even if other pages fail.
func (dao *NotionDao) ArchivePages(pageIds []notionapi.PageID) error {
	failedCount := 0
	for _, p := range pageIds {
		_, err := dao.client.Page.Update(
			context.TODO(),
			p,
			&notionapi.PageUpdateRequest{
				Archived:   true,
				Properties: notionapi.Properties{}, // Must be provided, even if empty
			},
		)
		if err != nil {
			fmt.Printf("Failed to archive page: %s. Error: %s\n", p.String(), err.Error())
			failedCount++
		}
	}
	if failedCount > 0 {
		return fmt.Errorf("failed to archive %d pages", failedCount)
	}
	return nil
}

// GetEnabledRssFeeds from the Feed Database. Results filtered on property "Enabled"=true
func (dao *NotionDao) GetEnabledRssFeeds() chan *FeedDatabaseItem {
	rssFeeds := make(chan *FeedDatabaseItem)

	go func(dao *NotionDao, output chan *FeedDatabaseItem) {
		defer close(output)

		req := &notionapi.DatabaseQueryRequest{
			Filter: notionapi.PropertyFilter{
				Property: "Enabled",
				Checkbox: &notionapi.CheckboxFilterCondition{
					Equals: true,
				},
			},
		}

		//TODO: Get multi-page pagination results from resp.HasMore
		resp, err := dao.client.Database.Query(context.Background(), dao.feedDatabaseId, req)
		if err != nil {
			return
		}
		for _, r := range resp.Results {
			feed, err := GetRssFeedFromDatabaseObject(&r)
			if err == nil {
				rssFeeds <- feed
			}
		}
	}(dao, rssFeeds)
	return rssFeeds
}

func GetRssFeedFromDatabaseObject(p *notionapi.Page) (*FeedDatabaseItem, error) {
	if p.Properties["Link"] == nil || p.Properties["Title"] == nil {
		return &FeedDatabaseItem{}, fmt.Errorf("notion page is expected to have `Link` and `Title` properties. Properties: %s", p.Properties)
	}
	urlProperty := p.Properties["Link"].(*notionapi.URLProperty).URL
	rssUrl, err := url.Parse(urlProperty)
	if err != nil {
		return &FeedDatabaseItem{}, err
	}

	nameRichTexts := p.Properties["Title"].(*notionapi.TitleProperty).Title
	if len(nameRichTexts) == 0 {
		return &FeedDatabaseItem{}, fmt.Errorf("RSS Feed database entry does not have any Title in 'Title' field")
	}

	return &FeedDatabaseItem{
		FeedLink:     rssUrl,
		Created:      p.CreatedTime,
		LastModified: p.LastEditedTime,
		Name:         nameRichTexts[0].PlainText,
	}, nil
}

func GetImageUrl(x string) *string {
	// Extract the first image src from the document to use as cover
	re := regexp.MustCompile(`(?m)<img\b[^>]+?src\s*=\s*['"]?([^\s'"?#>]+)`)
	match := re.FindSubmatch([]byte(x))
	if match != nil {
		v := string(match[1])
		if strings.HasPrefix(v, "http") {
			return &v
		} else {
			fmt.Printf("[ERROR]: Invalid image url found in <img> url=%s\n", string(match[1]))
			return nil
		}
	}
	return nil
}

// AddRssItem to Notion database as a single new page with Block content. On failure, no retry is attempted.
func (dao NotionDao) AddRssItem(item RssItem) error {
	categories := make([]notionapi.Option, len(item.categories))
	for i, c := range item.categories {
		categories[i] = notionapi.Option{
			Name: c,
		}
	}
	var imageProp *notionapi.Image
	// TODO: Currently notionapi.URLProperty is not nullable, which is needed
	//   to use thumbnail properly (i.e. handle the case when no image in RSS item).
	//thumbnailProp := &notionapi.URLProperty{
	//	Type: "url",
	//	URL: ,
	//}

	image := GetImageUrl(strings.Join(item.content, " "))
	if image != nil {
		imageProp = &notionapi.Image{
			Type: "external",
			External: &notionapi.FileObject{
				URL: *image,
			},
		}
	}
	// 定义要拼接的字符串
	stringsToConcat := []string{item.title,item.link.String(), item.published.String()}
	// 使用 strings.Join 进行字符串拼接
	concatenatedString := strings.Join(stringsToConcat, "")
	// 计算 md5 哈希值
	hasher := md5.New()
	hasher.Write([]byte(concatenatedString))
	hash := hex.EncodeToString(hasher.Sum(nil))
	// 检查元素是否在集合中
	if _, exists := dao.globalHash[hash]; exists {
		fmt.Printf("%s 在订阅列表里面了，且文章没有更新 hash:%s\n", item.link.String(),hash)
		return nil
	}
	// 去掉html
	description := removeHtmlAndGarbage(*item.description)

	// 创建URL 预览块
	children := []notionapi.Block{
		notionapi.EmbedBlock{
			BasicBlock: notionapi.BasicBlock{
				Object: notionapi.ObjectTypeBlock,
				Type:   notionapi.BlockTypeEmbed,
			},
			Embed: notionapi.Embed{
				URL: item.link.String(),
			},
		},
	}

	_, err := dao.client.Page.Create(context.Background(), &notionapi.PageCreateRequest{
		Parent: notionapi.Parent{
			Type:       "database_id",
			DatabaseID: dao.contentDatabaseId,
		},
		Properties: map[string]notionapi.Property{
			"Title": notionapi.TitleProperty{
				Type: "title",
				Title: []notionapi.RichText{{
					Type: "text",
					Text: notionapi.Text{
						Content: item.title,
					},
				}},
			},
			"Description": notionapi.RichTextProperty{
				Type: "rich_text",
				RichText: []notionapi.RichText{{
					Type: notionapi.ObjectTypeText,
					Text: notionapi.Text{
						Content: description,
					},
					PlainText: description,
				},
				},
			},
			"Link": notionapi.URLProperty{
				Type: "url",
				URL:  item.link.String(),
			},
			"Categories": notionapi.MultiSelectProperty{
				MultiSelect: categories,
			},
			"From":      notionapi.SelectProperty{Select: notionapi.Option{Name: item.feedName}},
			"Published": notionapi.DateProperty{Date: &notionapi.DateObject{Start: (*notionapi.Date)(item.published)}},
			"hash": notionapi.RichTextProperty{
				Type: "rich_text",
				RichText: []notionapi.RichText{{
					Type: notionapi.ObjectTypeText,
					Text: notionapi.Text{
						Content: hash,
					},
					PlainText: hash,
				},
				},
			},
		},
		Children: children,
		Cover:    imageProp,
	})
	return err
}

func RssContentToBlocks(item RssItem) []notionapi.Block {
	// TODO: implement when we know RssItem struct better
	return []notionapi.Block{}
}

func GetJinaAI(url string) (*JinaAIRes,error){
	client := &http.Client{}
	req, err := http.NewRequest("GET", fmt.Sprintf("https://r.jina.ai/%s",url), nil)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var response *JinaAIRes
	if err = json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	return response, nil
}

type JinaAIRes struct {
	Code   int `json:"code"`
	Status int `json:"status"`
	Data   struct {
		Title         string    `json:"title"`
		URL           string    `json:"url"`
		Content       string    `json:"content"`
		PublishedTime time.Time `json:"publishedTime"`
		Usage         struct {
			Tokens int `json:"tokens"`
		} `json:"usage"`
	} `json:"data"`
}


// splitText 将长文本分割为多个部分
func splitText(text string, maxLength int) []string {
	var parts []string
	for len(text) > maxLength {
		parts = append(parts, text[:maxLength])
		text = text[maxLength:]
	}
	parts = append(parts, text)
	return parts
}

func removeHtmlAndGarbage(input string) string {
	// 正则表达式匹配 HTML 标签
	re := regexp.MustCompile(`<[^>]*>`)
	cleaned := re.ReplaceAllString(input, "")

	// 去除多余的空格和乱码字符
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	cleaned = strings.ReplaceAll(cleaned, "\r", "")
	regex := regexp.MustCompile(`&#[0-9A-F]+;`)

	cleaned = strings.TrimSpace(cleaned)
	cleaned = regex.ReplaceAllString(cleaned, "")
	return cleaned
}