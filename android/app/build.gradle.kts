plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Release identity comes from the environment so CI can stamp a tag onto
// the APK exactly the way it stamps one onto the Go binary.
val releaseVersion: String = (System.getenv("KINDLED_VERSION") ?: "1.0").removePrefix("v")
val releaseVersionCode: Int = System.getenv("KINDLED_VERSION_CODE")?.toIntOrNull() ?: 1

// Signing is optional: with no keystore configured the release build is
// produced unsigned rather than failing, so a fork can build one without
// secrets. Set KEYSTORE_PATH and friends to get a signed, installable APK.
val keystorePath: String? = System.getenv("KEYSTORE_PATH")?.takeIf { it.isNotBlank() }

android {
    namespace = "com.kindled.display"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.kindled.display"
        // API 30: GestureDescription.Builder.setDisplayId, which is what
        // makes injecting touches into the virtual display possible.
        minSdk = 30
        targetSdk = 35
        versionCode = releaseVersionCode
        versionName = releaseVersion
    }

    signingConfigs {
        if (keystorePath != null) {
            create("release") {
                storeFile = file(keystorePath)
                storePassword = System.getenv("KEYSTORE_PASSWORD")
                keyAlias = System.getenv("KEY_ALIAS")
                keyPassword = System.getenv("KEY_PASSWORD")
            }
        }
    }

    // Two builds of the same app, differing only in whether the Kindle can
    // control the phone or only watch it.
    //
    // The mirror flavour exists because declaring an accessibility service
    // -- which is the only way to inject touches -- is by itself enough for
    // Play Protect to block a sideloaded install. Mirror carries no such
    // declaration, so it installs normally.
    flavorDimensions += "input"

    productFlavors {
        create("mirror") {
            dimension = "input"
        }
        create("control") {
            dimension = "input"
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            if (keystorePath != null) {
                signingConfig = signingConfigs.getByName("release")
            }
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    testOptions {
        unitTests.isReturnDefaultValues = true
    }
}

// No AndroidX, no Material, no third-party libraries: the UI is three
// buttons and a status line, and a dependency-free app is one less thing to
// go wrong in a hotel room with no internet.
dependencies {
    testImplementation("junit:junit:4.13.2")
}
