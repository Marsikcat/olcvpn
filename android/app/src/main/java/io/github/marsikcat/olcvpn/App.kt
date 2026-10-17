package io.github.marsikcat.olcvpn

import android.app.Application

class App : Application() {
    override fun onCreate() {
        super.onCreate()
        Backend.init(this)
        Net.watch(this)
        Alerts.channels(this)
    }
}
