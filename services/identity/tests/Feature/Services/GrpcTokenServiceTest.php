<?php

use App\Grpc\Auth\V1\IssueRequest;
use App\Grpc\Auth\V1\IssueResponse;
use App\Grpc\Auth\V1\VerifyRequest;
use App\Grpc\Auth\V1\VerifyResponse;
use App\Grpc\AuthClient;
use App\Services\GrpcTokenService;

// gRPC status codes the auth service can answer with.
const GRPC_OK = 0;
const GRPC_UNAUTHENTICATED = 16;

/**
 * Stand-in for the unary call a real client stub returns: ->wait() yields [response, status].
 */
function unaryCall(?object $response, int $code, string $details = ''): object
{
    return new class($response, $code, $details)
    {
        public function __construct(private ?object $response, private int $code, private string $details) {}

        /** @return array{0: ?object, 1: object} */
        public function wait(): array
        {
            return [$this->response, (object) ['code' => $this->code, 'details' => $this->details]];
        }
    };
}

test('issue returns the access token and its lifetime from the auth service', function () {
    $client = Mockery::mock(AuthClient::class);
    $client->shouldReceive('Issue')->once()
        ->withArgs(fn (IssueRequest $request) => $request->getSubject() === '42')
        ->andReturn(unaryCall((new IssueResponse)->setAccessToken('signed.jwt.token')->setExpiresIn(900), GRPC_OK));

    $result = (new GrpcTokenService($client, 2.0))->issue('42');

    expect($result)->toBe(['access_token' => 'signed.jwt.token', 'expires_in' => 900]);
});

test('issue throws when the auth service fails', function () {
    $client = Mockery::mock(AuthClient::class);
    $client->shouldReceive('Issue')->once()->andReturn(unaryCall(null, 14, 'connection refused'));

    expect(fn () => (new GrpcTokenService($client, 2.0))->issue('42'))
        ->toThrow(RuntimeException::class, 'Issue failed with gRPC status 14: connection refused');
});

test('verify returns the subject of a token the auth service accepts', function () {
    $client = Mockery::mock(AuthClient::class);
    $client->shouldReceive('Verify')->once()
        ->withArgs(fn (VerifyRequest $request) => $request->getAccessToken() === 'signed.jwt.token')
        ->andReturn(unaryCall((new VerifyResponse)->setSubject('42')->setExpiresAt(1794232780), GRPC_OK));

    $subject = (new GrpcTokenService($client, 2.0))->verify('signed.jwt.token');

    expect($subject)->toBe('42');
});

test('verify returns null when the auth service rejects the token', function () {
    $client = Mockery::mock(AuthClient::class);
    $client->shouldReceive('Verify')->once()
        ->andReturn(unaryCall(null, GRPC_UNAUTHENTICATED, 'token expired'));

    $subject = (new GrpcTokenService($client, 2.0))->verify('expired.jwt.token');

    expect($subject)->toBeNull();
});

test('verify throws instead of accepting the token when the auth service fails', function (int $code) {
    $client = Mockery::mock(AuthClient::class);
    $client->shouldReceive('Verify')->once()->andReturn(unaryCall(null, $code, 'boom'));

    expect(fn () => (new GrpcTokenService($client, 2.0))->verify('signed.jwt.token'))
        ->toThrow(RuntimeException::class, "Verify failed with gRPC status {$code}");
})->with([
    'unavailable' => 14,
    'deadline exceeded' => 4,
    'internal' => 13,
]);

test('verify returns null without calling the auth service when there is no token', function () {
    $client = Mockery::mock(AuthClient::class);
    $client->shouldNotReceive('Verify');

    $subject = (new GrpcTokenService($client, 2.0))->verify('');

    expect($subject)->toBeNull();
});

test('every call carries the configured deadline in microseconds', function () {
    $client = Mockery::mock(AuthClient::class);
    $client->shouldReceive('Issue')->once()
        ->withArgs(fn ($request, $metadata, $options) => $options === ['timeout' => 2_500_000])
        ->andReturn(unaryCall((new IssueResponse)->setAccessToken('t')->setExpiresIn(900), GRPC_OK));
    $client->shouldReceive('Verify')->once()
        ->withArgs(fn ($request, $metadata, $options) => $options === ['timeout' => 2_500_000])
        ->andReturn(unaryCall((new VerifyResponse)->setSubject('42'), GRPC_OK));
    $service = new GrpcTokenService($client, 2.5);

    $service->issue('42');
    $service->verify('t');
});
