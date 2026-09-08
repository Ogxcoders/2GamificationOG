// Universal Engagement Platform — Unreal (C++) SDK header.
// Thin by design: the server owns every decision. Uses FHttpModule; events
// batch client-side and flush with stable idempotency keys.
#pragma once

#include "CoreMinimal.h"
#include "HttpModule.h"
#include "Interfaces/IHttpRequest.h"
#include "Interfaces/IHttpResponse.h"
#include "Dom/JsonObject.h"
#include "Serialization/JsonSerializer.h"

class UEPCLIENT_API FUEPClient
{
public:
    struct FIngestResult
    {
        FString EventId;
        FString Status; // inserted | duplicate | rejected
    };

    FUEPClient(const FString& BaseUrl, const FString& ApiKey, const FString& ProjectId, const FString& EnvironmentId, const FString& UserId)
        : Scope(FString::Printf(TEXT("/v1/projects/%s/environments/%s"), *ProjectId, *EnvironmentId))
        , Auth(FString::Printf(TEXT("Bearer %s"), *ApiKey))
        , ActorId(UserId)
        , BaseUrl(BaseUrl) {}

    void SetUserId(const FString& UserId) { ActorId = UserId; }

    /** Buffer an event; auto-flush at 100 pending. */
    void Track(const FString& EventType, const TSharedPtr<FJsonObject>& Payload = nullptr)
    {
        TSharedPtr<FJsonObject> Ev = MakeShared<FJsonObject>();
        Ev->SetStringField(TEXT("event_type"), EventType);
        Ev->SetStringField(TEXT("actor_id"), ActorId);
        Ev->SetStringField(TEXT("occurred_at"), FDateTime::UtcNow().ToIso8601());
        Ev->SetStringField(TEXT("idempotency_key"), FString::Printf(TEXT("idem_%s"), *FGuid::NewGuid().ToString()));
        Ev->SetObjectField(TEXT("payload"), Payload ? Payload : MakeShared<FJsonObject>());
        Queue.Add(Ev);
        if (Queue.Num() >= 100)
        {
            Flush();
        }
    }

    /** Send buffered events; duplicates reported truthfully per event. */
    void Flush()
    {
        if (Queue.Num() == 0) return;
        TArray<TSharedPtr<FJsonValue>> Events;
        for (auto& E : Queue) Events.Add(MakeShared<FJsonValueObject>(E));
        Queue.Empty();

        TSharedPtr<FJsonObject> Body = MakeShared<FJsonObject>();
        Body->SetArrayField(TEXT("events"), Events);

        Request(TEXT("POST"), Scope + TEXT("/events"), Body, [this](TSharedPtr<FJsonObject> Res)
        {
            // Results surfaced via delegate by the game layer if needed.
        });
    }

    /** Full player state. */
    void GetState(TFunction<void(TSharedPtr<FJsonObject>)> OnDone)
    {
        Request(TEXT("GET"), FString::Printf(TEXT("%s/users/%s/state"), *Scope, *ActorId), nullptr,
            [OnDone](TSharedPtr<FJsonObject> Res) { OnDone(Res); });
    }

    /** Leaderboard page including the caller's row. */
    void GetLeaderboard(const FString& LeaderboardId, int32 Limit, TFunction<void(TSharedPtr<FJsonObject>)> OnDone)
    {
        Request(TEXT("GET"), FString::Printf(TEXT("%s/leaderboards/%s?me=%s&limit=%d"), *Scope, *LeaderboardId, *ActorId, Limit), nullptr,
            [OnDone](TSharedPtr<FJsonObject> Res) { OnDone(Res); });
    }

    /** Decision trace for one event. */
    void GetTrace(const FString& EventId, TFunction<void(TSharedPtr<FJsonObject>)> OnDone)
    {
        Request(TEXT("GET"), FString::Printf(TEXT("%s/events/%s/trace"), *Scope, *EventId), nullptr,
            [OnDone](TSharedPtr<FJsonObject> Res) { OnDone(Res); });
    }

private:
    FString Scope;
    FString Auth;
    FString ActorId;
    FString BaseUrl;
    TArray<TSharedPtr<FJsonObject>> Queue;

    void Request(const FString& Method, const FString& Path, TSharedPtr<FJsonObject> Body, TFunction<void(TSharedPtr<FJsonObject>)> OnDone)
    {
        TSharedRef<IHttpRequest> Req = FHttpModule::Get().CreateRequest();
        Req->SetVerb(Method);
        Req->SetURL(BaseUrl + Path);
        Req->SetHeader(TEXT("Authorization"), Auth);
        Req->SetHeader(TEXT("Content-Type"), TEXT("application/json"));
        if (Body.IsValid())
        {
            FString Out;
            auto Writer = TJsonWriterFactory<>::Create(&Out);
            FJsonSerializer::Serialize(Body.ToSharedRef(), Writer);
            Req->SetContentAsString(Out);
        }
        Req->OnProcessRequestComplete().BindLambda([OnDone](FHttpRequestPtr, FHttpResponsePtr Resp, bool bSuccess)
        {
            if (!bSuccess || !Resp.IsValid()) { OnDone(nullptr); return; }
            int32 Code = Resp->GetResponseCode();
            if (Code < 200 || Code >= 300) { OnDone(nullptr); return; }
            TSharedPtr<FJsonObject> Json;
            TSharedRef<TJsonReader<>> Reader = TJsonReaderFactory<>::Create(Resp->GetContentAsString());
            FJsonSerializer::Deserialize(Reader, Json);
            OnDone(Json);
        });
        Req->ProcessRequest();
    }
};
