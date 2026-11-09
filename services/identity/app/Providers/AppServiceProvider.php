<?php

namespace App\Providers;

use App\Grpc\AuthClient;
use App\Models\User;
use App\Services\GrpcTokenService;
use App\Services\TokenService;
use Grpc\ChannelCredentials;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Auth;
use Illuminate\Support\ServiceProvider;

class AppServiceProvider extends ServiceProvider
{
    /**
     * Register any application services.
     */
    public function register(): void
    {
        // The auth service listens on an internal port only; the mesh sidecar provides mTLS.
        $this->app->singleton(TokenService::class, fn () => new GrpcTokenService(
            new AuthClient(config('jwt.auth_addr'), ['credentials' => ChannelCredentials::createInsecure()]),
            config('jwt.auth_timeout'),
        ));
    }

    /**
     * Bootstrap any application services.
     */
    public function boot(): void
    {
        Auth::viaRequest('jwt', function (Request $request): ?User {
            $subject = $this->app->make(TokenService::class)->verify((string) $request->bearerToken());

            return $subject === null ? null : User::find($subject);
        });
    }
}
