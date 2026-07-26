package otpqueue

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeDDB struct {
	queryOut   *dynamodb.QueryOutput
	queryErr   error
	lastQuery  *dynamodb.QueryInput
	lastDelete *dynamodb.DeleteItemInput
	lastUpdate *dynamodb.UpdateItemInput
	updateErr  error
}

func (f *fakeDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.lastQuery = in
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.queryOut, nil
}

func (f *fakeDDB) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.lastDelete = in
	return &dynamodb.DeleteItemOutput{}, nil
}

func (f *fakeDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.lastUpdate = in
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func s(v string) types.AttributeValue  { return &types.AttributeValueMemberS{Value: v} }
func n(v string) types.AttributeValue  { return &types.AttributeValueMemberN{Value: v} }
func strOf(av types.AttributeValue) string {
	return av.(*types.AttributeValueMemberS).Value
}

// List must classify a MIXED partition — otp + welcome + unknown sk prefixes
// in one Query page — and ignore ElectroDB's __edb_e__/__edb_v__ meta noise.
func TestListClassifiesMixedPartition(t *testing.T) {
	fake := &fakeDDB{queryOut: &dynamodb.QueryOutput{
		Items: []map[string]types.AttributeValue{
			{
				"pk":        s("$run#queue_otp"),
				"sk":        s("$meshotppending_1#nodeid_!433d1cec"),
				"__edb_e__": s("MeshOtpPending"),
				"__edb_v__": s("1"),
				"queue":     s("otp"),
				"nodeId":    s("!433d1cec"),
				"nodeNum":   n("1128078572"),
				"code":      s("123456"),
				"publicKey": s("0xabc123"),
				"userId":    s("user-1"),
				"attempts":  n("2"),
				"createdAt": n("1784969443000"),
			},
			{
				"pk":        s("$run#queue_otp"),
				"sk":        s("$meshwelcomepending_1#nodeid_!433d1cec"),
				"__edb_e__": s("MeshWelcomePending"),
				"__edb_v__": s("1"),
				"queue":     s("otp"),
				"nodeId":    s("!433d1cec"),
				"nodeNum":   n("1128078572"),
				"message":   s("Welcome to defcon.run, Kurt!"),
				"userId":    s("user-1"),
				"attempts":  n("0"),
				"createdAt": n("1784969443001"),
			},
			{
				"pk": s("$run#queue_otp"),
				"sk": s("$somethingelse_1#nodeid_!433d1cec"),
			},
		},
	}}
	store := NewDynamoDBStoreWithClient(fake, "run-human-electro")

	items, welcomes, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || len(welcomes) != 1 {
		t.Fatalf("want 1 otp + 1 welcome, got %d/%d", len(items), len(welcomes))
	}
	it := items[0]
	if it.NodeID != "!433d1cec" || it.NodeNum != 1128078572 || it.Code != "123456" ||
		it.PublicKey != "0xabc123" || it.UserID != "user-1" || it.Attempts != 2 ||
		it.CreatedAt != 1784969443000 {
		t.Fatalf("bad otp unmarshal: %+v", it)
	}
	w := welcomes[0]
	if w.NodeID != "!433d1cec" || w.NodeNum != 1128078572 ||
		w.Message != "Welcome to defcon.run, Kurt!" || w.Attempts != 0 {
		t.Fatalf("bad welcome unmarshal: %+v", w)
	}
	// Query shape: whole constant partition — never a Scan, no sk condition.
	if kc := *fake.lastQuery.KeyConditionExpression; kc != "pk = :pk" {
		t.Fatalf("bad key condition: %q", kc)
	}
	if strOf(fake.lastQuery.ExpressionAttributeValues[":pk"]) != "$run#queue_otp" {
		t.Fatalf("bad :pk")
	}
}

func TestDeleteWelcomeComposesWelcomeKey(t *testing.T) {
	fake := &fakeDDB{}
	store := NewDynamoDBStoreWithClient(fake, "run-human-electro")
	if err := store.DeleteWelcome(context.Background(), "!433d1cec"); err != nil {
		t.Fatalf("DeleteWelcome: %v", err)
	}
	if strOf(fake.lastDelete.Key["sk"]) != "$meshwelcomepending_1#nodeid_!433d1cec" {
		t.Fatalf("bad welcome delete key: %+v", fake.lastDelete.Key)
	}
}

func TestDeleteComposesQueueKey(t *testing.T) {
	fake := &fakeDDB{}
	store := NewDynamoDBStoreWithClient(fake, "run-human-electro")
	if err := store.Delete(context.Background(), "!433d1cec"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if strOf(fake.lastDelete.Key["pk"]) != "$run#queue_otp" ||
		strOf(fake.lastDelete.Key["sk"]) != "$meshotppending_1#nodeid_!433d1cec" {
		t.Fatalf("bad delete key: %+v", fake.lastDelete.Key)
	}
}

func TestMarkRadioCodeSentGuardedAndSwallowsConditionFailure(t *testing.T) {
	fake := &fakeDDB{}
	store := NewDynamoDBStoreWithClient(fake, "run-human-electro")

	if err := store.MarkRadioCodeSent(context.Background(), 1128078572, 1784969443000); err != nil {
		t.Fatalf("MarkRadioCodeSent: %v", err)
	}
	if strOf(fake.lastUpdate.Key["pk"]) != "$run#nodeid_!433d1cec" ||
		strOf(fake.lastUpdate.Key["sk"]) != "$meshradio_1" {
		t.Fatalf("bad radio key: %+v", fake.lastUpdate.Key)
	}
	if *fake.lastUpdate.ConditionExpression != "attribute_exists(pk)" {
		t.Fatalf("missing orphan-row guard: %v", fake.lastUpdate.ConditionExpression)
	}

	// Deleted-radio race: conditional failure is silently OK.
	fake.updateErr = &types.ConditionalCheckFailedException{}
	if err := store.MarkRadioCodeSent(context.Background(), 1128078572, 1); err != nil {
		t.Fatalf("conditional failure must be swallowed, got %v", err)
	}

	// Any other error propagates.
	fake.updateErr = errors.New("throttled")
	if err := store.MarkRadioCodeSent(context.Background(), 1128078572, 1); err == nil {
		t.Fatal("real error must propagate")
	}
}
